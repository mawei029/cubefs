package fs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
)

type serveNodePoolTestNode struct{}

func (serveNodePoolTestNode) Attr(ctx context.Context, attr *fuse.Attr) error {
	attr.Mode = 0644
	return nil
}

type serveNodePoolTestLinker struct {
	serveNodePoolTestNode
	old Node
	err error
}

func (n *serveNodePoolTestLinker) Link(ctx context.Context, req *fuse.LinkRequest, old Node) (Node, error) {
	n.old = old
	return nil, n.err
}

type serveNodePoolTestRenamer struct {
	serveNodePoolTestNode
	newDir Node
	err    error
}

func (n *serveNodePoolTestRenamer) Rename(ctx context.Context, req *fuse.RenameRequest, newDir Node) error {
	n.newDir = newDir
	return n.err
}

func newServeNodePoolTestServer(node Node, sn *serveNode) *Server {
	server := New(nil, nil)
	server.node = []*serveNode{nil, sn}
	server.nodeRef[node] = 1
	return server
}

func TestReleaseServeNodeSkipsActiveRefs(t *testing.T) {
	node := serveNodePoolTestNode{}
	sn := newServeNode(10, 20, node, 1)

	releaseServeNode(nil)
	releaseServeNode(sn)
	if sn.node != node || sn.inode != 10 || sn.generation != 20 || sn.refs != 1 {
		t.Fatal("releaseServeNode cleared a serveNode with active refs")
	}

	sn.refs = 0
	releaseServeNode(sn)
	if sn.node != nil || sn.inode != 0 || sn.generation != 0 {
		t.Fatal("releaseServeNode did not clear a releasable serveNode")
	}
}

func TestPinNodeBoundsAndExistingNode(t *testing.T) {
	node := serveNodePoolTestNode{}
	sn := newServeNode(10, 20, node, 1)
	server := newServeNodePoolTestServer(node, sn)

	if _, _, ok := server.pinNode(2); ok {
		t.Fatal("pinNode pinned an out-of-range node")
	}
	if _, _, ok := server.pinNode(0); ok {
		t.Fatal("pinNode pinned a nil node slot")
	}

	pinnedNode, pinnedSNode, ok := server.pinNode(1)
	if !ok {
		t.Fatal("pinNode did not pin an existing node")
	}
	defer pinnedSNode.wg.Done()
	if pinnedNode != node || pinnedSNode != sn {
		t.Fatal("pinNode returned an unexpected node")
	}
}

func TestSaveNodeUsesPooledServeNode(t *testing.T) {
	node := serveNodePoolTestNode{}
	server := New(nil, nil)

	id, gen := server.saveNode(123, node)
	if id != 0 {
		t.Fatalf("unexpected node id %d", id)
	}
	if gen != 0 {
		t.Fatalf("unexpected generation %d", gen)
	}
	if server.node[id] == nil || server.node[id].node != node || server.node[id].inode != 123 {
		t.Fatal("saveNode did not install the new serveNode")
	}
}

func TestDropNodeKeepsServeNodeWithRemainingRefs(t *testing.T) {
	node := serveNodePoolTestNode{}
	sn := newServeNode(99, 7, node, 2)
	server := newServeNodePoolTestServer(node, sn)

	if server.dropNode(1, 1) {
		t.Fatal("dropNode forgot a node with remaining refs")
	}
	if sn.refs != 1 || server.node[1] != sn || sn.node != node {
		t.Fatal("dropNode changed a node that still had refs")
	}
}

func TestDropNodeHandlesInvalidAndMissingNodes(t *testing.T) {
	server := New(nil, nil)
	server.node = []*serveNode{nil}

	if !server.dropNode(2, 1) {
		t.Fatal("dropNode should report forget for an invalid node id")
	}
	if !server.dropNode(0, 1) {
		t.Fatal("dropNode should report forget for a missing node slot")
	}
}

func TestDropNodeWaitsForPinnedServeNodeBeforePooling(t *testing.T) {
	node := serveNodePoolTestNode{}
	sn := newServeNode(99, 7, node, 1)

	server := newServeNodePoolTestServer(node, sn)

	pinnedNode, pinnedSNode, ok := server.pinNode(1)
	if !ok {
		t.Fatal("expected pinNode to pin an existing serveNode")
	}
	if pinnedNode != node || pinnedSNode != sn {
		t.Fatal("pinNode returned an unexpected node")
	}

	dropped := make(chan bool, 1)
	go func() {
		dropped <- server.dropNode(1, 1)
	}()

	select {
	case <-dropped:
		t.Fatal("dropNode completed while serveNode was still pinned")
	case <-time.After(20 * time.Millisecond):
	}

	if sn.node == nil {
		t.Fatal("serveNode was cleared before pinned access was released")
	}

	pinnedSNode.wg.Done()

	select {
	case forget := <-dropped:
		if !forget {
			t.Fatal("expected dropNode to forget the last reference")
		}
	case <-time.After(time.Second):
		t.Fatal("dropNode did not complete after pinned access was released")
	}

	if sn.node != nil {
		t.Fatal("serveNode was not cleared after pinned access drained")
	}
	if _, _, ok := server.pinNode(1); ok {
		t.Fatal("removed node should not be pinnable")
	}
}

func TestCheckNodePinsRegularRequestsButNotForget(t *testing.T) {
	node := serveNodePoolTestNode{}
	sn := newServeNode(99, 7, node, 1)
	server := newServeNodePoolTestServer(node, sn)

	gotNode, gotSNode, _, done := server.checkNode(&fuse.GetattrRequest{
		Header: fuse.Header{Node: 1},
	}, &serveRequest{})
	if done {
		t.Fatal("checkNode unexpectedly completed a valid regular request")
	}
	if gotNode != node || gotSNode != sn {
		t.Fatal("checkNode returned an unexpected node for regular request")
	}
	gotSNode.wg.Done()

	gotNode, gotSNode, _, done = server.checkNode(&fuse.ForgetRequest{
		Header: fuse.Header{Node: 1},
		N:      1,
	}, &serveRequest{})
	if done {
		t.Fatal("checkNode unexpectedly completed a valid forget request")
	}
	if gotNode != node {
		t.Fatal("checkNode returned an unexpected node for forget request")
	}
	if gotSNode != nil {
		t.Fatal("forget requests should not pin their own serveNode")
	}
}

func TestLinkAndRenamePinSecondaryNodes(t *testing.T) {
	oldNode := serveNodePoolTestNode{}
	oldSNode := newServeNode(10, 20, oldNode, 1)
	linker := &serveNodePoolTestLinker{err: errors.New("link failed")}
	server := newServeNodePoolTestServer(oldNode, oldSNode)

	err := server.handleRequest(context.Background(), linker, nil, &fuse.LinkRequest{
		Header:  fuse.Header{Node: 1},
		OldNode: 1,
	}, func(resp interface{}) {})
	if err == nil || err.Error() != "link failed" {
		t.Fatalf("unexpected link error %v", err)
	}
	if linker.old != oldNode {
		t.Fatal("Link did not receive the pinned old node")
	}

	newDirNode := serveNodePoolTestNode{}
	newDirSNode := newServeNode(30, 40, newDirNode, 1)
	renamer := &serveNodePoolTestRenamer{err: errors.New("rename failed")}
	server = newServeNodePoolTestServer(newDirNode, newDirSNode)

	err = server.handleRequest(context.Background(), renamer, nil, &fuse.RenameRequest{
		Header: fuse.Header{Node: 1},
		NewDir: 1,
	}, func(resp interface{}) {})
	if err == nil || err.Error() != "rename failed" {
		t.Fatalf("unexpected rename error %v", err)
	}
	if renamer.newDir != newDirNode {
		t.Fatal("Rename did not receive the pinned new directory node")
	}
}
