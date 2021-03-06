package client

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/utils"
)

// CmdExec runs a command in a running container.
//
// Usage: docker exec [OPTIONS] CONTAINER COMMAND [ARG...]
func (cli *DockerCli) CmdExec(args ...string) error {
	cmd := cli.Subcmd("exec", "CONTAINER COMMAND [ARG...]", "Run a command in a running container", true)

	execConfig, err := runconfig.ParseExec(cmd, args)
	// just in case the ParseExec does not exit
	if execConfig.Container == "" || err != nil {
		return &utils.StatusError{StatusCode: 1}
	}

	stream, _, err := cli.call("POST", "/containers/"+execConfig.Container+"/exec", execConfig, nil)
	if err != nil {
		return err
	}

	var response types.ContainerExecCreateResponse
	if err := json.NewDecoder(stream).Decode(&response); err != nil {
		return err
	}
	for _, warning := range response.Warnings {
		fmt.Fprintf(cli.err, "WARNING: %s\n", warning)
	}

	execID := response.ID

	if execID == "" {
		fmt.Fprintf(cli.out, "exec ID empty")
		return nil
	}

	if !execConfig.Detach {
		if err := cli.CheckTtyInput(execConfig.AttachStdin, execConfig.Tty); err != nil {
			return err
		}
	} else {
		if _, _, err := readBody(cli.call("POST", "/exec/"+execID+"/start", execConfig, nil)); err != nil {
			return err
		}
		// For now don't print this - wait for when we support exec wait()
		// fmt.Fprintf(cli.out, "%s\n", execID)
		return nil
	}

	// Interactive exec requested.
	var (
		out, stderr io.Writer
		in          io.ReadCloser
		hijacked    = make(chan io.Closer)
		errCh       chan error
	)

	// Block the return until the chan gets closed
	defer func() {
		logrus.Debugf("End of CmdExec(), Waiting for hijack to finish.")
		if _, ok := <-hijacked; ok {
			logrus.Errorf("Hijack did not finish (chan still open)")
		}
	}()

	if execConfig.AttachStdin {
		in = cli.in
	}
	if execConfig.AttachStdout {
		out = cli.out
	}
	if execConfig.AttachStderr {
		if execConfig.Tty {
			stderr = cli.out
		} else {
			stderr = cli.err
		}
	}
	errCh = promise.Go(func() error {
		return cli.hijack("POST", "/exec/"+execID+"/start", execConfig.Tty, in, out, stderr, hijacked, execConfig)
	})

	// Acknowledge the hijack before starting
	select {
	case closer := <-hijacked:
		// Make sure that hijack gets closed when returning. (result
		// in closing hijack chan and freeing server's goroutines.
		if closer != nil {
			defer closer.Close()
		}
	case err := <-errCh:
		if err != nil {
			logrus.Debugf("Error hijack: %s", err)
			return err
		}
	}

	if execConfig.Tty && cli.isTerminalIn {
		if err := cli.monitorTtySize(execID, true); err != nil {
			logrus.Errorf("Error monitoring TTY size: %s", err)
		}
	}

	if err := <-errCh; err != nil {
		logrus.Debugf("Error hijack: %s", err)
		return err
	}

	var status int
	if _, status, err = getExecExitCode(cli, execID); err != nil {
		return err
	}

	if status != 0 {
		return &utils.StatusError{StatusCode: status}
	}

	return nil
}
