package service

const Kind = "systemd user service"

func Install(opts Options) (*Installation, error) { return installSystemd(opts) }
func Uninstall() (bool, error)                    { return uninstallSystemd() }
func Installed() bool                             { return systemdInstalled() }
