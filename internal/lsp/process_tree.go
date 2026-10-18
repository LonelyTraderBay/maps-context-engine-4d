package lsp

import "os/exec"

type processTreeGuard interface {
	Attach(*exec.Cmd) error
	Kill() error
	Close() error
}
