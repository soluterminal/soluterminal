//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"unsafe"
)

// readPpid answers from the Toolhelp snapshot; refresh it so a process spawned
// a moment ago is in it.
func refreshProcessTreeForCaller() { _ = freshProcessTreeSnapshot() }

// mibTcpStateEstab is MIB_TCP_STATE_ESTAB.
const mibTcpStateEstab = 5

// lookupPeerPIDPlatform finds the process owning the client end of a local
// TCP connection from the owner-pid connection table.
func lookupPeerPIDPlatform(client, server *net.TCPAddr) (int, bool) {
	if c4, s4 := client.IP.To4(), server.IP.To4(); c4 != nil && s4 != nil {
		if pid := findTcp4Owner(c4, client.Port, s4, server.Port); pid > 0 {
			return pid, true
		}
	}
	if pid := findTcp6Owner(client.IP.To16(), client.Port, server.IP.To16(), server.Port); pid > 0 {
		return pid, true
	}
	return 0, false
}

// tablePort converts the network-byte-order port in the low 16 bits of a row.
func tablePort(v uint32) int {
	return int(uint16(v&0xff)<<8 | uint16((v>>8)&0xff))
}

func findTcp4Owner(clientIP net.IP, clientPort int, serverIP net.IP, serverPort int) int {
	buf, err := getExtendedTcpTableClass(afInet, tcpTableOwnerPidAll)
	if err != nil || len(buf) < 4 {
		return 0
	}
	n := binary.LittleEndian.Uint32(buf[:4])
	rows := buf[4:]
	rowSize := int(unsafe.Sizeof(mibTcpRowOwnerPid{}))
	for i := 0; i < int(n); i++ {
		start := i * rowSize
		if start+rowSize > len(rows) {
			break
		}
		r := *(*mibTcpRowOwnerPid)(unsafe.Pointer(&rows[start]))
		if r.State != mibTcpStateEstab || tablePort(r.LocalPort) != clientPort || tablePort(r.RemotePort) != serverPort {
			continue
		}
		if net.ParseIP(ipv4Format(r.LocalAddr)).Equal(clientIP) && net.ParseIP(ipv4Format(r.RemoteAddr)).Equal(serverIP) {
			return int(r.OwningPid)
		}
	}
	return 0
}

func findTcp6Owner(clientIP net.IP, clientPort int, serverIP net.IP, serverPort int) int {
	if clientIP == nil || serverIP == nil {
		return 0
	}
	buf, err := getExtendedTcpTableClass(afInet6, tcpTableOwnerPidAll)
	if err != nil || len(buf) < 4 {
		return 0
	}
	n := binary.LittleEndian.Uint32(buf[:4])
	rows := buf[4:]
	rowSize := int(unsafe.Sizeof(mibTcp6RowOwnerPid{}))
	for i := 0; i < int(n); i++ {
		start := i * rowSize
		if start+rowSize > len(rows) {
			break
		}
		r := *(*mibTcp6RowOwnerPid)(unsafe.Pointer(&rows[start]))
		if r.State != mibTcpStateEstab || tablePort(r.LocalPort) != clientPort || tablePort(r.RemotePort) != serverPort {
			continue
		}
		if bytes.Equal(r.LocalAddr[:], clientIP) && bytes.Equal(r.RemoteAddr[:], serverIP) {
			return int(r.OwningPid)
		}
	}
	return 0
}

// tracedOutsiderJudgedUntraced: on Windows a traced program outside every pane
// and worker that isn't a known browser is the person only with an approved
// device's signature. A variable so tests can apply either platform's rule.
var tracedOutsiderJudgedUntraced = false

// platformBrowserProcessNames are Windows browsers' exact executable names
// (lower-cased, without .exe): Arc, Zen and LibreWolf.
var platformBrowserProcessNames = map[string]bool{"arc": true, "zen": true, "librewolf": true}
