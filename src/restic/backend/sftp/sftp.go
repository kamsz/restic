package sftp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"restic"
	"strings"
	"time"

	"restic/errors"

	"restic/backend"
	"restic/debug"

	"github.com/pkg/sftp"
)

// SFTP is a backend in a directory accessed via SFTP.
type SFTP struct {
	c *sftp.Client
	p string

	cmd    *exec.Cmd
	result <-chan error

	backend.Layout
	Config
}

var _ restic.Backend = &SFTP{}

const defaultLayout = "default"

func startClient(program string, args ...string) (*SFTP, error) {
	debug.Log("start client %v %v", program, args)
	// Connect to a remote host and request the sftp subsystem via the 'ssh'
	// command.  This assumes that passwordless login is correctly configured.
	cmd := exec.Command(program, args...)

	// prefix the errors with the program name
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StderrPipe")
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			fmt.Fprintf(os.Stderr, "subprocess %v: %v\n", program, sc.Text())
		}
	}()

	// ignore signals sent to the parent (e.g. SIGINT)
	cmd.SysProcAttr = ignoreSigIntProcAttr()

	// get stdin and stdout
	wr, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StdinPipe")
	}
	rd, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StdoutPipe")
	}

	// start the process
	if err := cmd.Start(); err != nil {
		return nil, errors.Wrap(err, "cmd.Start")
	}

	// wait in a different goroutine
	ch := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		debug.Log("ssh command exited, err %v", err)
		ch <- errors.Wrap(err, "cmd.Wait")
	}()

	// open the SFTP session
	client, err := sftp.NewClientPipe(rd, wr)
	if err != nil {
		return nil, errors.Errorf("unable to start the sftp session, error: %v", err)
	}

	return &SFTP{c: client, cmd: cmd, result: ch}, nil
}

// clientError returns an error if the client has exited. Otherwise, nil is
// returned immediately.
func (r *SFTP) clientError() error {
	select {
	case err := <-r.result:
		debug.Log("client has exited with err %v", err)
		return err
	default:
	}

	return nil
}

// Open opens an sftp backend as described by the config by running
// "ssh" with the appropriate arguments (or cfg.Command, if set).
func Open(cfg Config) (*SFTP, error) {
	debug.Log("open backend with config %#v", cfg)

	cmd, args, err := buildSSHCommand(cfg)
	if err != nil {
		return nil, err
	}

	sftp, err := startClient(cmd, args...)
	if err != nil {
		debug.Log("unable to start program: %v", err)
		return nil, err
	}

	sftp.Layout, err = backend.ParseLayout(sftp, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	debug.Log("layout: %v\n", sftp.Layout)

	// create paths for data and refs. mkdirAll does nothing if the paths already exist.
	for _, d := range sftp.Paths() {
		err = sftp.mkdirAll(d, backend.Modes.Dir)
		debug.Log("mkdirAll %v -> %v", d, err)
		if err != nil {
			return nil, err
		}
	}

	sftp.Config = cfg
	sftp.p = cfg.Path
	return sftp, nil
}

// Join combines path components with slashes (according to the sftp spec).
func (r *SFTP) Join(p ...string) string {
	return path.Join(p...)
}

// ReadDir returns the entries for a directory.
func (r *SFTP) ReadDir(dir string) ([]os.FileInfo, error) {
	return r.c.ReadDir(dir)
}

// IsNotExist returns true if the error is caused by a not existing file.
func (r *SFTP) IsNotExist(err error) bool {
	if os.IsNotExist(err) {
		return true
	}

	statusError, ok := err.(*sftp.StatusError)
	if !ok {
		return false
	}

	return statusError.Error() == `sftp: "No such file" (SSH_FX_NO_SUCH_FILE)`
}

func buildSSHCommand(cfg Config) (cmd string, args []string, err error) {
	if cfg.Command != "" {
		return SplitShellArgs(cfg.Command)
	}

	cmd = "ssh"

	hostport := strings.Split(cfg.Host, ":")
	args = []string{hostport[0]}
	if len(hostport) > 1 {
		args = append(args, "-p", hostport[1])
	}
	if cfg.User != "" {
		args = append(args, "-l")
		args = append(args, cfg.User)
	}
	args = append(args, "-s")
	args = append(args, "sftp")
	return cmd, args, nil
}

// Create creates an sftp backend as described by the config by running
// "ssh" with the appropriate arguments (or cfg.Command, if set).
func Create(cfg Config) (*SFTP, error) {
	cmd, args, err := buildSSHCommand(cfg)
	if err != nil {
		return nil, err
	}

	sftp, err := startClient(cmd, args...)
	if err != nil {
		debug.Log("unable to start program: %v", err)
		return nil, err
	}

	sftp.Layout, err = backend.ParseLayout(sftp, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	// test if config file already exists
	_, err = sftp.c.Lstat(Join(cfg.Path, backend.Paths.Config))
	if err == nil {
		return nil, errors.New("config file already exists")
	}

	// create paths for data and refs
	for _, d := range sftp.Paths() {
		err = sftp.mkdirAll(d, backend.Modes.Dir)
		debug.Log("mkdirAll %v -> %v", d, err)
		if err != nil {
			return nil, err
		}
	}

	err = sftp.Close()
	if err != nil {
		return nil, errors.Wrap(err, "Close")
	}

	// open backend
	return Open(cfg)
}

// Location returns this backend's location (the directory name).
func (r *SFTP) Location() string {
	return r.p
}

func (r *SFTP) mkdirAll(dir string, mode os.FileMode) error {
	// check if directory already exists
	fi, err := r.c.Lstat(dir)
	if err == nil {
		if fi.IsDir() {
			return nil
		}

		return errors.Errorf("mkdirAll(%s): entry exists but is not a directory", dir)
	}

	// create parent directories
	errMkdirAll := r.mkdirAll(path.Dir(dir), backend.Modes.Dir)

	// create directory
	errMkdir := r.c.Mkdir(dir)

	// test if directory was created successfully
	fi, err = r.c.Lstat(dir)
	if err != nil {
		// return previous errors
		return errors.Errorf("mkdirAll(%s): unable to create directories: %v, %v", dir, errMkdirAll, errMkdir)
	}

	if !fi.IsDir() {
		return errors.Errorf("mkdirAll(%s): entry exists but is not a directory", dir)
	}

	// set mode
	return r.c.Chmod(dir, mode)
}

// Join joins the given paths and cleans them afterwards. This always uses
// forward slashes, which is required by sftp.
func Join(parts ...string) string {
	return path.Clean(path.Join(parts...))
}

// Save stores data in the backend at the handle.
func (r *SFTP) Save(ctx context.Context, h restic.Handle, rd io.Reader) (err error) {
	debug.Log("Save %v", h)
	if err := r.clientError(); err != nil {
		return err
	}

	if err := h.Valid(); err != nil {
		return err
	}

	filename := r.Filename(h)

	// create new file
	f, err := r.c.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if r.IsNotExist(errors.Cause(err)) {
		// create the locks dir, then try again
		err = r.mkdirAll(r.Dirname(h), backend.Modes.Dir)
		if err != nil {
			return errors.Wrap(err, "MkdirAll")
		}

		return r.Save(ctx, h, rd)
	}

	if err != nil {
		return errors.Wrap(err, "OpenFile")
	}

	// save data
	_, err = io.Copy(f, rd)
	if err != nil {
		_ = f.Close()
		return errors.Wrap(err, "Write")
	}

	err = f.Close()
	if err != nil {
		return errors.Wrap(err, "Close")
	}

	// set mode to read-only
	fi, err := r.c.Lstat(filename)
	if err != nil {
		return errors.Wrap(err, "Lstat")
	}

	err = r.c.Chmod(filename, fi.Mode()&os.FileMode(^uint32(0222)))
	return errors.Wrap(err, "Chmod")
}

// Load returns a reader that yields the contents of the file at h at the
// given offset. If length is nonzero, only a portion of the file is
// returned. rd must be closed after use.
func (r *SFTP) Load(ctx context.Context, h restic.Handle, length int, offset int64) (io.ReadCloser, error) {
	debug.Log("Load %v, length %v, offset %v", h, length, offset)
	if err := h.Valid(); err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, errors.New("offset is negative")
	}

	f, err := r.c.Open(r.Filename(h))
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	if length > 0 {
		return backend.LimitReadCloser(f, int64(length)), nil
	}

	return f, nil
}

// Stat returns information about a blob.
func (r *SFTP) Stat(ctx context.Context, h restic.Handle) (restic.FileInfo, error) {
	debug.Log("Stat(%v)", h)
	if err := r.clientError(); err != nil {
		return restic.FileInfo{}, err
	}

	if err := h.Valid(); err != nil {
		return restic.FileInfo{}, err
	}

	fi, err := r.c.Lstat(r.Filename(h))
	if err != nil {
		return restic.FileInfo{}, errors.Wrap(err, "Lstat")
	}

	return restic.FileInfo{Size: fi.Size()}, nil
}

// Test returns true if a blob of the given type and name exists in the backend.
func (r *SFTP) Test(ctx context.Context, h restic.Handle) (bool, error) {
	debug.Log("Test(%v)", h)
	if err := r.clientError(); err != nil {
		return false, err
	}

	_, err := r.c.Lstat(r.Filename(h))
	if os.IsNotExist(errors.Cause(err)) {
		return false, nil
	}

	if err != nil {
		return false, errors.Wrap(err, "Lstat")
	}

	return true, nil
}

// Remove removes the content stored at name.
func (r *SFTP) Remove(ctx context.Context, h restic.Handle) error {
	debug.Log("Remove(%v)", h)
	if err := r.clientError(); err != nil {
		return err
	}

	return r.c.Remove(r.Filename(h))
}

// List returns a channel that yields all names of blobs of type t. A
// goroutine is started for this. If the channel done is closed, sending
// stops.
func (r *SFTP) List(ctx context.Context, t restic.FileType) <-chan string {
	debug.Log("List %v", t)

	ch := make(chan string)

	go func() {
		defer close(ch)

		walker := r.c.Walk(r.Basedir(t))
		for walker.Step() {
			if walker.Err() != nil {
				continue
			}

			if !walker.Stat().Mode().IsRegular() {
				continue
			}

			select {
			case ch <- path.Base(walker.Path()):
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch

}

var closeTimeout = 2 * time.Second

// Close closes the sftp connection and terminates the underlying command.
func (r *SFTP) Close() error {
	debug.Log("")
	if r == nil {
		return nil
	}

	err := r.c.Close()
	debug.Log("Close returned error %v", err)

	// wait for closeTimeout before killing the process
	select {
	case err := <-r.result:
		return err
	case <-time.After(closeTimeout):
	}

	if err := r.cmd.Process.Kill(); err != nil {
		return err
	}

	// get the error, but ignore it
	<-r.result
	return nil
}
