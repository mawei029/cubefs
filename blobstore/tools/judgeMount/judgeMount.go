package main

import (
	"fmt"
	"github.com/deniswernert/go-fstab"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) != 3 {
		path, _ := os.Executable()
		_, exec := filepath.Split(path)
		fmt.Printf("usage of %s : <mode> <path> \n", exec) // os.Args[0]
		return
	}

	mode := os.Args[1]
	path := os.Args[2]
	flag := false
	var err error
	// mode := "3"  // debug
	// path := "/mnt/hgfs"

	switch mode {
	case "1":
		listArgs()
		return
	case "2":
		flag = IsMountPoint(path) // blobnode
	case "3":
		flag = isMountPoint2(path)
	case "4":
		flag, err = DiskMounted(path, "") // kodo
	}
	fmt.Printf("is mount point:%v , err:%+v \n", flag, err)
}

func listArgs() {
	for idx, arg := range os.Args {
		fmt.Printf("args[%d] : %s \n", idx, arg)
	}
}

func IsMountPoint(path string) bool {
	var stat1, stat2 os.FileInfo

	stat1, err := os.Lstat(path)
	if err != nil {
		return false
	}
	stat2, err = os.Lstat(filepath.Dir(strings.TrimSuffix(path, "/")))
	if err != nil {
		return false
	}
	if stat1.Mode()&os.ModeSymlink != 0 {
		return false
	}

	dev1 := stat1.Sys().(*syscall.Stat_t).Dev
	dev2 := stat2.Sys().(*syscall.Stat_t).Dev

	inode1 := stat1.Sys().(*syscall.Stat_t).Ino
	inode2 := stat2.Sys().(*syscall.Stat_t).Ino

	if dev1 != dev2 || inode1 == inode2 {
		return true
	}

	return false
}

func isMountPoint2(directory string) bool {
	fileInfo, err := os.Lstat(directory)
	if err != nil {
		fmt.Printf("Error getting information for directory %s: %s\n", directory, err)
		return false
	}
	if fileInfo.IsDir() {
		// 检查是否有文件系统类型为“挂载点”的文件存在
		mountPointsPath := filepath.Join(directory, "mtab") // mtab文件包含目录的挂载信息
		_, err = os.Stat(mountPointsPath)
		if err == nil {
			return true
		} else if os.IsNotExist(err) {
			// 挂载点文件不存在，继续检查父目录
			parentDirectory := filepath.Dir(directory)
			if parentDirectory == directory {
				// 已经到达根目录，不是挂载点
				return false
			}
			return isMountPoint2(parentDirectory)
		} else {
			fmt.Printf("Error checking mtab file for directory %s: %s\n", directory, err)
			return false
		}
	} else {
		fmt.Printf("%s is not a directory.\n", directory)
		return false
	}
}

func GetEtcFstab(path string) (ok bool, err error) {
	mounts, err := fstab.ParseSystem()
	if err != nil {
		return
	}
	for _, mount := range mounts {
		if mount.File != "/" && strings.HasPrefix(path, mount.File) {
			ok = true
			return
		}
	}
	err = fmt.Errorf("can not found disk %s in /etc/fstab", path)
	return
}

func GetProcMounts(path string) (ok bool, err error) {
	mounts, err := fstab.ParseProc()
	if err != nil {
		return
	}
	for _, mount := range mounts {
		if mount.File != "/" && path == mount.File { // strings.HasPrefix(path, mount.File) {
			fmt.Printf("path:%s, mout.File:%s \n", path, mount.File)
			fmt.Printf("mounts:%+v \n", mounts)
			ok = true
			return
		}
	}
	err = fmt.Errorf("can not found disk %s in /proc/mounts", path)
	return
}

func DiskMounted(path, prefix string) (bool, error) {
	//if os.Getenv("TRAVIS_BUILD_DIR") != "" || runtime.GOOS != "linux" {
	//	return true, nil
	//}
	if ok, err2 := GetEtcFstab(path); !ok || err2 != nil {
		return false, err2
	}
	path = filepath.Join(prefix, path)
	if ok, err2 := GetProcMounts(path); !ok || err2 != nil {
		return false, err2
	}
	return true, nil
}
