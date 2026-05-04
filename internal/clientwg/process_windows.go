//go:build windows

package clientwg

func brokerProcessAlive(pid int) bool {
	return false
}
