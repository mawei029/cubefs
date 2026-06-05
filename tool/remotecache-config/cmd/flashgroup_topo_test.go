package cmd

import (
	"testing"

	"github.com/cubefs/cubefs/proto"
)

func TestFlashGroupNodeAdd_topoNameFlag(t *testing.T) {
	cmd := newCmdFlashGroupNodeAdd(nil)
	flag := cmd.Flags().Lookup("topoName")
	if flag == nil {
		t.Fatal("nodeAdd command missing topoName flag")
	}
	if flag.Shorthand != "n" {
		t.Fatalf("topoName shorthand = %q, want n", flag.Shorthand)
	}
	if flag.DefValue != proto.DefaultTopoName {
		t.Fatalf("topoName default = %q, want %q", flag.DefValue, proto.DefaultTopoName)
	}
}
