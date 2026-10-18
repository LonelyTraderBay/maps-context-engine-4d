package exec

import (
	"os/exec"
	"time"
)

type processTreeGuard interface {
	Attach(*exec.Cmd) error
	ParentExited() (bool, error)
	RSSBytes() (int64, error)
	Kill() error
	Close() error
}

func monitorProcessTree(tree processTreeGuard, stopped <-chan struct{}, budget int64, interval time.Duration, recordPeak func(int64), terminate func(string), settle func()) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopped:
			return nil
		default:
		}
		exited, err := tree.ParentExited()
		if err != nil {
			select {
			case <-stopped:
				return nil
			default:
			}
			terminate("rss_measurement")
			return err
		}
		if exited {
			settle()
			return nil
		}
		bytes, err := tree.RSSBytes()
		if err != nil {
			select {
			case <-stopped:
				return nil
			default:
			}
			terminate("rss_measurement")
			return err
		}
		recordPeak(bytes)
		if bytes > budget {
			terminate("rss_limit")
			return nil
		}
		select {
		case <-stopped:
			return nil
		case <-ticker.C:
		}
	}
}
