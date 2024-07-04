package test

import (
	"os"
	"path/filepath"
	"time"

	log "k8s.io/klog/v2"

	. "github.com/qiniu/logkit/utils/models"
)

var (
	Dir      = "logdir"
	MetaDir  = "./meta"
	Files    = []string{"f3", "f2", "f1"}
	Contents = []string{"223456789", "123456789", "123456789"}
)

// CreateDir creates a new directory with default permission.
// It does not create missing parent directories and will return error if any parent directory does not exist.
func CreateDir() {
	err := os.Mkdir(Dir, DefaultDirPerm)
	if err != nil {
		log.Error(err)
		return
	}
}

// DestroyDir removes all directories that are created during testing.
func DestroyDir() {
	os.RemoveAll(Dir)
	os.RemoveAll(MetaDir)
}

// CreateFile creates an empty file in given path.
func CreateFile(fileName string, content string) {
	f, _ := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY, DefaultFilePerm)
	f.WriteString(content)
	f.Sync()
	f.Close()
}

// CreateFiles creates dummy files with dummy content.
// It delays given interval in milliseconds between creation of each file.
// It does not create missing parent directories and will return error if any parent directory does not exist.
func CreateFiles(interval int) {
	for i, f := range Files {
		file, err := os.OpenFile(filepath.Join(Dir, f), os.O_CREATE|os.O_WRONLY, DefaultFilePerm)
		if err != nil {
			log.Error(err)
			return
		}

		file.WriteString(Contents[i])
		file.Close()
		time.Sleep(time.Millisecond * time.Duration(interval))
	}
}

// DeleteFile removes file with given path.
func DeleteFile(fpath string) {
	os.RemoveAll(fpath)
}
