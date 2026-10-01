//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// isAdmin reports whether rtelegram runs elevated, as installing a Windows
// service requires.
func isAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

func installWindowsService(exe string, args []string) error {
	if !isAdmin() {
		return errors.New("installing a Windows service needs an administrator: run rtelegram -install from an elevated prompt")
	}
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer manager.Disconnect()

	service, err := manager.OpenService(serviceName)
	if err == nil {
		// Reinstalling points the existing service at the new settings.
		defer service.Close()
		if err := stopWindowsService(service); err != nil {
			return err
		}
		config, err := service.Config()
		if err != nil {
			return err
		}
		config.BinaryPathName = syscall.EscapeArg(exe)
		for _, arg := range args {
			config.BinaryPathName += " " + syscall.EscapeArg(arg)
		}
		config.StartType, config.DelayedAutoStart = mgr.StartAutomatic, true
		if err := service.UpdateConfig(config); err != nil {
			return fmt.Errorf("update the %s service: %w", serviceName, err)
		}
	} else {
		service, err = manager.CreateService(serviceName, exe, mgr.Config{
			DisplayName:      "rtelegram",
			Description:      "Telegram bot for rTorrent",
			StartType:        mgr.StartAutomatic,
			DelayedAutoStart: true,
		}, args...)
		if err != nil {
			return fmt.Errorf("create the %s service: %w", serviceName, err)
		}
		defer service.Close()
	}
	// Restart after any failure, such as rTorrent being down at boot.
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 15 * time.Second}},
		uint32((24 * time.Hour).Seconds())); err != nil {
		return err
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	return service.Start()
}

func uninstallWindowsService() error {
	if !isAdmin() {
		return errors.New("removing a Windows service needs an administrator: run rtelegram -uninstall from an elevated prompt")
	}
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("no %s service is installed: %w", serviceName, err)
	}
	defer service.Close()
	if err := stopWindowsService(service); err != nil {
		return err
	}
	return service.Delete()
}

// stopWindowsService stops service if it runs, and waits for it to stop.
func stopWindowsService(service *mgr.Service) error {
	status, err := service.Query()
	if err != nil || status.State == svc.Stopped {
		return err
	}
	if _, err := service.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop the %s service: %w", serviceName, err)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if status, err = service.Query(); err != nil || status.State == svc.Stopped {
			return err
		}
	}
	return fmt.Errorf("the %s service did not stop within 30s", serviceName)
}

// runAsService runs rtelegram under the Windows service manager, when that is
// what started it, and reports whether it did.
func runAsService() bool {
	inService, err := svc.IsWindowsService()
	if err != nil || !inService {
		return false
	}
	svc.Run(serviceName, windowsService{})
	return true
}

type windowsService struct{}

// Execute runs rtelegram until the service manager stops it. A service has no
// console, so output goes to rtelegram.log next to the config file.
func (windowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	output := io.Discard
	if path := serviceConfigPath(os.Args[1:]); path != "" {
		if file, err := os.OpenFile(filepath.Join(filepath.Dir(path), "rtelegram.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			defer file.Close()
			output = file
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, os.Args[1:], os.Getenv, output, output) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				fmt.Fprintln(output, time.Now().Format(time.DateTime), "rtelegram:", err)
				// A failure exit makes the service manager restart it.
				return false, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

// serviceConfigPath returns the -config value in args.
func serviceConfigPath(args []string) string {
	for i, arg := range args {
		switch {
		case (arg == "-config" || arg == "--config") && i+1 < len(args):
			return args[i+1]
		case len(arg) > len("-config=") && arg[:len("-config=")] == "-config=":
			return arg[len("-config="):]
		}
	}
	return ""
}
