//go:build windows

package api

import "net"

func netDialUDP(addr string) (net.Conn, error) { return net.Dial("udp", addr) }

func readDisk(path string) (total, used uint64) { return 0, 0 }
