// Copyright (C) 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remotessh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"

	"github.com/google/gapid/core/app/crash"
	"github.com/google/gapid/core/event/task"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/core/os/shell"
	"github.com/google/gapid/core/text"
	"golang.org/x/crypto/ssh"
)

// remoteProcess is the interface to a running process, as started by a Target.
type remoteProcess struct {
	session *ssh.Session
	wg      sync.WaitGroup
}

func (r *remoteProcess) Kill() error {
	return r.session.Signal(ssh.SIGSEGV)
}

func (r *remoteProcess) Wait(ctx context.Context) error {
	ret := r.session.Wait()
	r.wg.Wait()
	return ret
}

var _ shell.Process = (*remoteProcess)(nil)

type sshShellTarget struct{ b *binding }

// Start starts the given command in the remote shell.
func (t sshShellTarget) Start(cmd shell.Cmd) (shell.Process, error) {
	session, err := t.b.connection.NewSession()
	if err != nil {
		return nil, err
	}
	p := &remoteProcess{
		session: session,
		wg:      sync.WaitGroup{},
	}

	if cmd.Stdin != nil {
		stdin, err := session.StdinPipe()
		if err != nil {
			return nil, err
		}
		crash.Go(func() {
			defer stdin.Close()
			io.Copy(stdin, cmd.Stdin)
		})
	}

	if cmd.Stdout != nil {
		stdout, err := session.StdoutPipe()
		if err != nil {
			return nil, err
		}
		p.wg.Add(1)
		crash.Go(func() {
			io.Copy(cmd.Stdout, stdout)
			p.wg.Done()
		})
	}

	if cmd.Stderr != nil {
		stderr, err := session.StderrPipe()
		if err != nil {
			return nil, err
		}
		p.wg.Add(1)
		crash.Go(func() {
			io.Copy(cmd.Stderr, stderr)
			p.wg.Done()
		})
	}

	prefix := ""
	if cmd.Dir != "" {
		prefix += "cd " + cmd.Dir + "; "
	}

	for _, e := range cmd.Environment.Keys() {
		if e != "" {
			val := text.Quote([]string{cmd.Environment.Get(e)})[0]
			prefix = prefix + strings.TrimSpace(e) + "=" + val + " "
		}
	}

	for _, e := range t.b.env.Keys() {
		if e != "" {
			val := text.Quote([]string{t.b.env.Get(e)})[0]
			prefix = prefix + strings.TrimSpace(e) + "=" + val + " "
		}
	}

	val := prefix + cmd.Name + " " + strings.Join(cmd.Args, " ")
	if err := session.Start(val); err != nil {
		return nil, err
	}

	return p, nil
}

func (t sshShellTarget) String() string {
	c := t.b.configuration
	return c.User + "@" + c.Host + ": " + t.b.String()
}

// Shell implements the Device interface returning commands that will error if run.
func (b binding) Shell(name string, args ...string) shell.Cmd {
	return shell.Command(name, args...).On(sshShellTarget{&b})
}

func (b binding) destroyPosixDirectory(ctx context.Context, dir string) {
	_, _ = b.Shell("rm", "-rf", dir).Call(ctx)
}

func (b binding) createPosixTempDirectory(ctx context.Context) (string, func(context.Context), error) {
	dir, err := b.Shell("mktemp", "-d").Call(ctx)
	if err != nil {
		return "", nil, err
	}
	return dir, func(ctx context.Context) { b.destroyPosixDirectory(ctx, dir) }, nil
}

func (b binding) createWindowsTempDirectory(ctx context.Context) (string, func(ctx context.Context), error) {
	return "", nil, fmt.Errorf("Windows remote targets are not yet supported.")
}

// MakeTempDir creates a temporary directory on the remote machine. It returns the
// full path, and a function that can be called to clean up the directory.
func (b binding) MakeTempDir(ctx context.Context) (string, func(ctx context.Context), error) {
	switch b.os {
	case device.Linux, device.OSX:
		return b.createPosixTempDirectory(ctx)
	case device.Windows:
		return b.createWindowsTempDirectory(ctx)
	default:
		panic(fmt.Errorf("Unsupported OS %v", b.os))
	}
}

// WriteFile moves the contents of io.Reader into the given file on the remote machine.
// The file is given the mode as described by the unix filemode string.
func (b binding) WriteFile(ctx context.Context, contents io.Reader, mode os.FileMode, destPath string) error {
	perm := fmt.Sprintf("%4o", mode.Perm())
	_, err := b.Shell("cat", ">", destPath, "; chmod ", perm, " ", destPath).Read(contents).Call(ctx)
	return err
}

// PushFile copies a file from a local path to the remote machine. Permissions are
// maintained across.
func (b binding) PushFile(ctx context.Context, source, dest string) error {
	infile, err := os.Open(source)
	if err != nil {
		return err
	}
	permission, err := os.Stat(source)
	if err != nil {
		return err
	}
	mode := permission.Mode()
	// If we are on windows pushing to Linux, we lose the executable
	// bit, get it back.
	if (b.os == device.Linux ||
		b.os == device.OSX) &&
		runtime.GOOS == "windows" {
		mode |= 0550
	}

	return b.WriteFile(ctx, infile, mode, dest)
}

// doTunnel tunnels a single connection through the SSH connection.
func (b binding) doTunnel(ctx context.Context, local net.Conn, remotePort int) error {
	remote, err := b.connection.Dial("tcp", fmt.Sprintf("localhost:%d", remotePort))
	if err != nil {
		local.Close()
		return err
	}

	wg := sync.WaitGroup{}

	copy := func(writer net.Conn, reader net.Conn) {
		// Use the same buffer size used in io.Copy
		buf := make([]byte, 32*1024)
		var err error
		for {
			nr, er := reader.Read(buf)
			if nr > 0 {
				nw, ew := writer.Write(buf[0:nr])
				if ew != nil {
					err = ew
					break
				}
				if nr != nw {
					err = fmt.Errorf("short write")
					break
				}
			}
			if er != nil {
				if er != io.EOF {
					err = er
				}
				break
			}
		}
		writer.Close()
		if err != nil {
			log.E(ctx, "Copy Error %s", err)
		}
		wg.Done()
	}

	wg.Add(2)
	crash.Go(func() { copy(local, remote) })
	crash.Go(func() { copy(remote, local) })

	crash.Go(func() {
		defer local.Close()
		defer remote.Close()
		wg.Wait()
	})
	return nil
}

// SetupLocalPort forwards a local TCP port to the remote machine on the remote port.
// The local port that was opened is returned.
func (b binding) SetupLocalPort(ctx context.Context, remotePort int) (int, error) {
	listener, err := net.Listen("tcp", ":0")

	if err != nil {
		return 0, err
	}
	crash.Go(func() {
		<-task.ShouldStop(ctx)
		listener.Close()
	})
	crash.Go(func() {
		defer listener.Close()
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			if err = b.doTunnel(ctx, local, remotePort); err != nil {
				return
			}
		}
	})

	return listener.Addr().(*net.TCPAddr).Port, nil
}

// TempFile creates a temporary file on the given Device. It returns the
// path to the file, and a function that can be called to clean it up.
func (b binding) TempFile(ctx context.Context) (string, func(ctx context.Context), error) {
	res, err := b.Shell("mktemp").Call(ctx)
	if err != nil {
		return "", nil, err
	}
	return res, func(ctx context.Context) {
		b.Shell("rm", "-f", res).Call(ctx)
	}, nil
}

// FileContents returns the contents of a given file on the Device.
func (b binding) FileContents(ctx context.Context, path string) (string, error) {
	return b.Shell("cat", path).Call(ctx)
}

// RemoveFile removes the given file from the device
func (b binding) RemoveFile(ctx context.Context, path string) error {
	_, err := b.Shell("rm", "-f", path).Call(ctx)
	return err
}

// GetEnv returns the default environment for the Device.
func (b binding) GetEnv(ctx context.Context) (*shell.Env, error) {
	env, err := b.Shell("env").Call(ctx)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(strings.NewReader(env))
	e := shell.NewEnv()
	for scanner.Scan() {
		e.Add(scanner.Text())
	}
	return e, nil
}

// ListExecutables returns the executables in a particular directory as given by path
func (b binding) ListExecutables(ctx context.Context, inPath string) ([]string, error) {
	if inPath == "" {
		inPath = b.GetURIRoot()
	}
	// 'find' may partially succeed. Redirect the error messages to /dev/null,
	// only process the successfully found executables.
	files, _ := b.Shell("find", `"`+inPath+`"`, "-mindepth", "1", "-maxdepth", "1", "-type", "f", "-executable", "-printf", `%f\\n`, "2>/dev/null").Call(ctx)
	scanner := bufio.NewScanner(strings.NewReader(files))
	out := []string{}
	for scanner.Scan() {
		_, file := path.Split(scanner.Text())
		out = append(out, file)
	}
	return out, nil
}

// ListDirectories returns a list of directories rooted at a particular path
func (b binding) ListDirectories(ctx context.Context, inPath string) ([]string, error) {
	if inPath == "" {
		inPath = b.GetURIRoot()
	}
	// 'find' may partially succeed. Redirect the error messages to /dev/null,
	// only process the successfully found directories.
	dirs, _ := b.Shell("find", `"`+inPath+`"`, "-mindepth", "1", "-maxdepth", "1", "-type", "d", "-printf", `%f\\n`, "2>/dev/null").Call(ctx)
	scanner := bufio.NewScanner(strings.NewReader(dirs))
	out := []string{}
	for scanner.Scan() {
		_, file := path.Split(scanner.Text())
		out = append(out, file)
	}
	return out, nil
}

// IsFile returns true if the given path is a file
func (b binding) IsFile(ctx context.Context, inPath string) (bool, error) {
	dir, err := b.IsDirectory(ctx, inPath)
	if err == nil && dir {
		return false, nil
	}
	_, err = b.Shell("stat", `"`+inPath+`"`).Call(ctx)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// IsDirectory returns true if the given path is a directory
func (b binding) IsDirectory(ctx context.Context, inPath string) (bool, error) {
	_, err := b.Shell("cd", `"`+inPath+`"`).Call(ctx)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// GetWorkingDirectory returns the directory that this device considers CWD
func (b binding) GetWorkingDirectory(ctx context.Context) (string, error) {
	return b.Shell("pwd").Call(ctx)
}

func (b binding) GetURIRoot() string {
	return "/"
}
