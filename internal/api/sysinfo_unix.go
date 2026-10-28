//go:build !windows

package api

import (
	"net"
	"syscall"
)

// netDialUDP 建一个不会实际发数据的 UDP socket,只看本地出口地址。
func netDialUDP(addr string) (net.Conn, error) {
	return net.Dial("udp", addr)
}

// readDisk 用 statfs 读取指定挂载点容量(字节)。不依赖 df 命令。
func readDisk(path string) (total, used uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	free := st.Bavail * bsize
	if total >= free {
		used = total - free
	}
	return total, used
}
