//go:build !darwin && !linux

package service

const Kind = "background service"

func Install(Options) (*Installation, error) { return nil, ErrUnsupported }
func Uninstall() (bool, error)               { return false, ErrUnsupported }
func Installed() bool                        { return false }
