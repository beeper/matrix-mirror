package service

const Kind = "launchd agent"

func Install(opts Options) (*Installation, error) { return installLaunchd(opts) }
func Uninstall() (bool, error)                    { return uninstallLaunchd() }
func Installed() bool                             { return launchdInstalled() }
