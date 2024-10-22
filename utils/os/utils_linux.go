package os

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	log "k8s.io/klog/v2"
)

func GetOSInfo() *OSInfo {
	out := _getInfo()
	tryTime := 0
	for strings.Index(out, "broken pipe") != -1 {
		if tryTime > 3 {
			break
		}

		time.Sleep(500 * time.Millisecond)
		out = _getInfo()
		tryTime++
	}
	osStr := strings.Replace(out, "\n", "", -1)
	osStr = strings.Replace(osStr, "\r\n", "", -1)
	osInfo := strings.Split(osStr, " ")
	for i := len(osInfo); i < 4; i++ {
		osInfo = append(osInfo, "unknown")
	}
	gio := &OSInfo{Kernel: osInfo[0], Core: osInfo[1], Platform: runtime.GOARCH, OS: osInfo[3]}
	gio.Hostname, _ = os.Hostname()
	return gio
}

func _getInfo() string {
	cmd := exec.Command("uname", "-srio")
	cmd.Stdin = strings.NewReader("some input")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		log.Error("getInfo:", err)
	}
	return out.String()
}
