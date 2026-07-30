//go:build windows

package main

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/sys/windows/svc"
)

const windowsRuntimeServiceName = "SubmuxRuntime"

func runPlatformService(arguments []string, stderr io.Writer) (bool, int) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return true, 1
	}
	if !isService {
		return false, 0
	}
	if len(arguments) > 0 && arguments[0] == "serve" {
		arguments = arguments[1:]
	}
	handler := windowsRuntimeService{
		arguments: append([]string(nil), arguments...),
		stderr:    stderr,
	}
	if err := svc.Run(windowsRuntimeServiceName, handler); err != nil {
		fmt.Fprintln(stderr, err)
		return true, 1
	}
	return true, 0
}

type windowsRuntimeService struct {
	arguments []string
	stderr    io.Writer
}

func (service windowsRuntimeService) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	changes chan<- svc.Status,
) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	go func() {
		result <- runServeContext(ctx, service.arguments, service.stderr)
	}()
	changes <- svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}
	for {
		select {
		case code := <-result:
			if code != 0 {
				return false, uint32(code)
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				code := <-result
				if code != 0 {
					return false, uint32(code)
				}
				return false, 0
			}
		}
	}
}
