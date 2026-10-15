//go:build !linux

package execution

// InitCgroup is a no-op outside Linux. Windows uses Job Objects; other
// platforms still reject nonzero Limits in Start.
func InitCgroup() error { return nil }
