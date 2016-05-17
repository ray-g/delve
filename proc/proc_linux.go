package proc

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	sys "golang.org/x/sys/unix"
)

// Process statuses
const (
	StatusSleeping  = 'S'
	StatusRunning   = 'R'
	StatusTraceStop = 't'
	StatusZombie    = 'Z'

	// Kernel 2.6 has TraceStop as T
	// TODO(derekparker) Since this means something different based on the
	// version of the kernel ('T' is job control stop on modern 3.x+ kernels) we
	// may want to differentiate at some point.
	StatusTraceStopT = 'T'
)

// OSProcessDetails contains Linux specific
// process details.
type OSProcessDetails struct {
	comm string
}

// Launch creates and begins debugging a new process. First entry in
// `cmd` is the program to run, and then rest are the arguments
// to be supplied to that process.
func Launch(cmd []string) (*Process, error) {
	var (
		proc *exec.Cmd
		err  error
	)
	dbp := New(0)
	execOnPtraceThread(func() {
		proc = exec.Command(cmd[0])
		proc.Args = cmd
		proc.Stdout = os.Stdout
		proc.Stderr = os.Stderr
		proc.SysProcAttr = &syscall.SysProcAttr{Ptrace: true, Setpgid: true}
		err = proc.Start()
	})
	if err != nil {
		return nil, err
	}
	dbp.Pid = proc.Process.Pid
	_, _, err = dbp.wait(proc.Process.Pid, 0)
	if err != nil {
		return nil, fmt.Errorf("waiting for target execve failed: %s", err)
	}
	return initializeDebugProcess(dbp, proc.Path, false)
}

// Attach to an existing process with the given PID.
func Attach(pid int) (*Process, error) {
	return initializeDebugProcess(New(pid), "", true)
}

// Kill kills the target process.
func (dbp *Process) Kill() (err error) {
	if dbp.exited {
		return nil
	}
	if !dbp.Threads[dbp.Pid].Stopped() {
		return errors.New("process must be stopped in order to kill it")
	}
	if err = sys.Kill(-dbp.Pid, sys.SIGKILL); err != nil {
		return errors.New("could not deliver signal " + err.Error())
	}
	if _, _, err = dbp.wait(dbp.Pid, 0); err != nil {
		return
	}
	dbp.postExit()
	return
}

func requestManualStop(dbp *Process) (err error) {
	return sys.Kill(dbp.Pid, sys.SIGTRAP)
}

// Attach to a newly created thread, and store that thread in our list of
// known threads.
func (dbp *Process) addThread(tid int, attach bool) (*Thread, error) {
	if thread, ok := dbp.Threads[tid]; ok {
		return thread, nil
	}

	var err error
	if attach {
		execOnPtraceThread(func() { err = sys.PtraceAttach(tid) })
		if err != nil && err != sys.EPERM {
			// Do not return err if err == EPERM,
			// we may already be tracing this thread due to
			// PTRACE_O_TRACECLONE. We will surely blow up later
			// if we truly don't have permissions.
			return nil, fmt.Errorf("could not attach to new thread %d %s", tid, err)
		}
		pid, status, err := dbp.wait(tid, 0)
		if err != nil {
			return nil, err
		}
		if status.Exited() {
			return nil, fmt.Errorf("thread already exited %d", pid)
		}
	}

	execOnPtraceThread(func() { err = syscall.PtraceSetOptions(tid, syscall.PTRACE_O_TRACECLONE) })
	if err == syscall.ESRCH {
		if _, _, err = dbp.wait(tid, 0); err != nil {
			return nil, fmt.Errorf("error while waiting after adding thread: %d %s", tid, err)
		}
		execOnPtraceThread(func() { err = syscall.PtraceSetOptions(tid, syscall.PTRACE_O_TRACECLONE) })
		if err == syscall.ESRCH {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("could not set options for new traced thread %d %s", tid, err)
		}
	}

	dbp.Threads[tid] = &Thread{
		ID:  tid,
		dbp: dbp,
		os:  new(OSSpecificDetails),
	}
	if dbp.CurrentThread == nil {
		dbp.SwitchThread(tid)
	}
	return dbp.Threads[tid], nil
}

func updateThreadList(dbp *Process) error {
	tids, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*", dbp.Pid))
	for _, tidpath := range tids {
		tidstr := filepath.Base(tidpath)
		tid, err := strconv.Atoi(tidstr)
		if err != nil {
			return err
		}
		if _, err := dbp.addThread(tid, tid != dbp.Pid); err != nil {
			return err
		}
	}
	return nil
}

func (dbp *Process) findExecutable(path string) (string, error) {
	if path == "" {
		path = fmt.Sprintf("/proc/%d/exe", dbp.Pid)
	}
	return path, nil
}

func (dbp *Process) trapWait(pid int) (*Thread, error) {
	for {
		wpid, status, err := dbp.wait(pid, 0)
		if err != nil {
			return nil, fmt.Errorf("wait err %s %d", err, pid)
		}
		log.Println("signal:", status.StopSignal(), status.TrapCause())
		if wpid == 0 {
			continue
		}
		th, ok := dbp.Threads[wpid]
		if ok {
			th.Status = (*WaitStatus)(status)
		}
		if status.Exited() {
			if wpid == dbp.Pid {
				dbp.postExit()
				return nil, ProcessExitedError{Pid: wpid, Status: status.ExitStatus()}
			}
			delete(dbp.Threads, wpid)
			continue
		}
		if status.StopSignal() == sys.SIGTRAP && status.TrapCause() == sys.PTRACE_EVENT_CLONE {
			// A traced thread has cloned a new thread, grab the pid and
			// add it to our list of traced threads.
			var cloned uint
			execOnPtraceThread(func() { cloned, err = sys.PtraceGetEventMsg(wpid) })
			if err != nil {
				return nil, fmt.Errorf("could not get event message: %s", err)
			}
			th, err = dbp.addThread(int(cloned), false)
			if err != nil {
				if err == sys.ESRCH {
					// thread died while we were adding it
					continue
				}
				return nil, err
			}
			if err = th.Continue(); err != nil {
				if err == sys.ESRCH {
					// thread died while we were adding it
					delete(dbp.Threads, th.ID)
					continue
				}
				return nil, fmt.Errorf("could not continue new thread %d %s", cloned, err)
			}
			if err = dbp.Threads[int(wpid)].Continue(); err != nil {
				if err != sys.ESRCH {
					return nil, fmt.Errorf("could not continue existing thread %d %s", wpid, err)
				}
			}
			continue
		}
		if th == nil {
			// Sometimes we get an unknown thread, ignore it?
			continue
		}
		// Drain
		for {
			wpid, _, err := dbp.wait(pid, sys.WNOHANG)
			if err != nil {
				return nil, err
			}
			if wpid == 0 {
				break
			}
		}
		switch status.StopSignal() {
		case sys.SIGTRAP:
			th.running = false
			return th, nil
		default:
			// TODO(derekparker) alert user about unexpected signals here.
			if err := th.resumeWithSig(int(status.StopSignal())); err != nil {
				if err == sys.ESRCH {
					return nil, ProcessExitedError{Pid: dbp.Pid}
				}
				return nil, err
			}
		}
	}
}

func (dbp *Process) loadProcessInformation() {
	comm, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/comm", dbp.Pid))
	if err != nil {
		fmt.Printf("Could not read process comm name: %v\n", err)
		os.Exit(1)
	}
	// removes newline character
	comm = comm[:len(comm)-1]
	dbp.os.comm = strings.Replace(string(comm), "%", "%%", -1)
}

func status(pid int, comm string) rune {
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return '\000'
	}
	defer f.Close()

	var (
		p     int
		state rune
	)

	// The second field of /proc/pid/stat is the name of the task in parenthesis.
	// The name of the task is the base name of the executable for this process limited to TASK_COMM_LEN characters
	// Since both parenthesis and spaces can appear inside the name of the task and no escaping happens we need to read the name of the executable first
	// See: include/linux/sched.c:315 and include/linux/sched.c:1510
	fmt.Fscanf(f, "%d ("+comm+")  %c", &p, &state)
	return state
}

func (dbp *Process) wait(pid, options int) (int, *sys.WaitStatus, error) {
	var s sys.WaitStatus
	if (pid != dbp.Pid) || (options != 0) {
		wpid, err := sys.Wait4(pid, &s, sys.WALL|options, nil)
		return wpid, &s, err
	}
	// If we call wait4/waitpid on a thread that is the leader of its group,
	// with options == 0, while ptracing and the thread leader has exited leaving
	// zombies of its own then waitpid hangs forever this is apparently intended
	// behaviour in the linux kernel because it's just so convenient.
	// Therefore we call wait4 in a loop with WNOHANG, sleeping a while between
	// calls and exiting when either wait4 succeeds or we find out that the thread
	// has become a zombie.
	// References:
	// https://sourceware.org/bugzilla/show_bug.cgi?id=12702
	// https://sourceware.org/bugzilla/show_bug.cgi?id=10095
	// https://sourceware.org/bugzilla/attachment.cgi?id=5685
	for {
		wpid, err := sys.Wait4(pid, &s, sys.WNOHANG|sys.WALL|options, nil)
		if err != nil {
			return 0, nil, err
		}
		if wpid != 0 {
			return wpid, &s, err
		}
		if status(pid, dbp.os.comm) == StatusZombie {
			return pid, nil, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (dbp *Process) setCurrentBreakpoints() error {
	for _, th := range dbp.Threads {
		if th.CurrentBreakpoint == nil {
			err := th.SetCurrentBreakpoint()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (dbp *Process) exitGuard(err error) error {
	if err != sys.ESRCH {
		return err
	}
	if status(dbp.Pid, dbp.os.comm) == StatusZombie {
		_, err := dbp.trapWait(-1)
		return err
	}

	return err
}

func (dbp *Process) resume() error {
	// all threads stopped over a breakpoint are made to step over it
	for _, thread := range dbp.Threads {
		if thread.CurrentBreakpoint != nil {
			if err := thread.StepInstruction(); err != nil {
				return err
			}
			thread.CurrentBreakpoint = nil
		}
	}
	// everything is resumed
	for _, thread := range dbp.Threads {
		if err := thread.resume(); err != nil && err != sys.ESRCH {
			return err
		}
	}
	return nil
}

func killProcess(pid int) error {
	return sys.Kill(pid, sys.SIGINT)
}
