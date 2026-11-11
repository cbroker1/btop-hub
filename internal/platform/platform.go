// Package platform holds the few OS-specific process concerns the agent needs:
// lowering its own priority, measuring its own resource use, and running as a service.
//
// Each OS file provides:
//
//	func SetLowPriority() error
//	func SelfStats() (cpu time.Duration, rss int64, err error) // CPU time used so far, resident bytes
//	func RunService(name string, run func(ctx context.Context) error) (isService bool, err error)
//	func LogPath() string // where a service writes its log, "" for stderr
//	func TrimMemory()     // release start-up-only pages from the working set (Windows)
package platform
