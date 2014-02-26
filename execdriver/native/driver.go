package native

import (
	"encoding/json"
	"fmt"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/pkg/cgroups"
	"github.com/dotcloud/docker/pkg/libcontainer"
	"github.com/dotcloud/docker/pkg/libcontainer/nsinit"
	"github.com/dotcloud/docker/pkg/system"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	DriverName = "native"
	Version    = "0.1"
)

func init() {
	execdriver.RegisterInitFunc(DriverName, func(args *execdriver.InitArgs) error {
		var (
			container *libcontainer.Container
			ns        = nsinit.NewNsInit(&nsinit.DefaultCommandFactory{}, &nsinit.DefaultStateWriter{args.Root})
		)
		f, err := os.Open(filepath.Join(args.Root, "container.json"))
		if err != nil {
			return err
		}
		if err := json.NewDecoder(f).Decode(&container); err != nil {
			f.Close()
			return err
		}
		f.Close()

		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		syncPipe, err := nsinit.NewSyncPipeFromFd(0, uintptr(args.Pipe))
		if err != nil {
			return err
		}
		if err := ns.Init(container, cwd, args.Console, syncPipe, args.Args); err != nil {
			return err
		}
		return nil
	})
}

type driver struct {
	root string
}

func NewDriver(root string) (*driver, error) {
	if err := os.MkdirAll(root, 0655); err != nil {
		return nil, err
	}
	return &driver{
		root: root,
	}, nil
}

func (d *driver) Run(c *execdriver.Command, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (int, error) {
	var (
		term        nsinit.Terminal
		container   = createContainer(c)
		factory     = &dockerCommandFactory{c: c, driver: d}
		stateWriter = &dockerStateWriter{
			callback: startCallback,
			c:        c,
			dsw:      &nsinit.DefaultStateWriter{filepath.Join(d.root, c.ID)},
		}
		ns   = nsinit.NewNsInit(factory, stateWriter)
		args = append([]string{c.Entrypoint}, c.Arguments...)
	)
	if err := d.createContainerRoot(c.ID); err != nil {
		return -1, err
	}
	defer d.removeContainerRoot(c.ID)

	if c.Tty {
		term = &dockerTtyTerm{
			pipes: pipes,
		}
	} else {
		term = &dockerStdTerm{
			pipes: pipes,
		}
	}
	c.Terminal = term
	if err := d.writeContainerFile(container, c.ID); err != nil {
		return -1, err
	}
	return ns.Exec(container, term, args)
}

func (d *driver) Kill(p *execdriver.Command, sig int) error {
	return syscall.Kill(p.Process.Pid, syscall.Signal(sig))
}

func (d *driver) Restore(c *execdriver.Command) error {
	var (
		nspid int
		path  = filepath.Join(d.root, c.ID, "pid")
	)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if _, err := fmt.Fscanf(f, "%d", &nspid); err != nil {
		f.Close()
		return err
	}
	f.Close()
	defer os.Remove(path)

	proc, err := os.FindProcess(nspid)
	if err != nil {
		return err
	}
	_, err = proc.Wait()
	return err
}

func (d *driver) Info(id string) execdriver.Info {
	return &info{
		ID:     id,
		driver: d,
	}
}

func (d *driver) Name() string {
	return fmt.Sprintf("%s-%s", DriverName, Version)
}

// TODO: this can be improved with our driver
// there has to be a better way to do this
func (d *driver) GetPidsForContainer(id string) ([]int, error) {
	pids := []int{}

	subsystem := "devices"
	cgroupRoot, err := cgroups.FindCgroupMountpoint(subsystem)
	if err != nil {
		return pids, err
	}
	cgroupDir, err := cgroups.GetThisCgroupDir(subsystem)
	if err != nil {
		return pids, err
	}

	filename := filepath.Join(cgroupRoot, cgroupDir, id, "tasks")
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		filename = filepath.Join(cgroupRoot, cgroupDir, "docker", id, "tasks")
	}

	output, err := ioutil.ReadFile(filename)
	if err != nil {
		return pids, err
	}
	for _, p := range strings.Split(string(output), "\n") {
		if len(p) == 0 {
			continue
		}
		pid, err := strconv.Atoi(p)
		if err != nil {
			return pids, fmt.Errorf("Invalid pid '%s': %s", p, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func (d *driver) writeContainerFile(container *libcontainer.Container, id string) error {
	data, err := json.Marshal(container)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(d.root, id, "container.json"), data, 0655)
}

func (d *driver) createContainerRoot(id string) error {
	return os.MkdirAll(filepath.Join(d.root, id), 0655)
}

func (d *driver) removeContainerRoot(id string) error {
	return os.RemoveAll(filepath.Join(d.root, id))
}

func getEnv(key string, env []string) string {
	for _, pair := range env {
		parts := strings.Split(pair, "=")
		if parts[0] == key {
			return parts[1]
		}
	}
	return ""
}

type dockerCommandFactory struct {
	c      *execdriver.Command
	driver *driver
}

// createCommand will return an exec.Cmd with the Cloneflags set to the proper namespaces
// defined on the container's configuration and use the current binary as the init with the
// args provided
func (d *dockerCommandFactory) Create(container *libcontainer.Container, console string, syncFd uintptr, args []string) *exec.Cmd {
	// we need to join the rootfs because nsinit will setup the rootfs and chroot
	initPath := filepath.Join(d.c.Rootfs, d.c.InitPath)

	d.c.Path = initPath
	d.c.Args = append([]string{
		initPath,
		"-driver", DriverName,
		"-console", console,
		"-pipe", fmt.Sprint(syncFd),
		"-root", filepath.Join(d.driver.root, d.c.ID),
	}, args...)

	// set this to nil so that when we set the clone flags anything else is reset
	d.c.SysProcAttr = nil
	system.SetCloneFlags(&d.c.Cmd, uintptr(nsinit.GetNamespaceFlags(container.Namespaces)))

	d.c.Env = container.Env
	d.c.Dir = d.c.Rootfs

	return &d.c.Cmd
}

type dockerStateWriter struct {
	dsw      nsinit.StateWriter
	c        *execdriver.Command
	callback execdriver.StartCallback
}

func (d *dockerStateWriter) WritePid(pid int) error {
	err := d.dsw.WritePid(pid)
	if d.callback != nil {
		d.callback(d.c)
	}
	return err
}

func (d *dockerStateWriter) DeletePid() error {
	return d.dsw.DeletePid()
}
