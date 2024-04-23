package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Minimal sandbox using isolate, adapted from the source code of kilonova.ro

const (
	runErrRetries = 3
	runErrTimeout = 200 * time.Millisecond
)

var IsolatePath = "/usr/local/bin/isolate" // Set up from install script

// IsolateBox is the struct for the current box
type IsolateBox struct {
	// the mutex makes sure we don't do anything stupid while we do other stuff
	mu    sync.Mutex
	path  string
	boxID int
}

// buildRunFlags compiles all flags into an array
func (b *IsolateBox) buildRunFlags(c *RunConfig, metaFile *os.File) (res []string) {
	res = append(res, "--box-id="+strconv.Itoa(b.boxID))

	res = append(res, "--cg", "--processes")
	for _, dir := range c.Directories {
		if dir.Removes {
			res = append(res, "--dir="+dir.In+"=")
			continue
		}
		toAdd := "--dir="
		toAdd += dir.In
		if dir.Out == "" {
			if !dir.Verbatim {
				toAdd += "=" + dir.In
			}
		} else {
			toAdd += "=" + dir.Out
		}
		if dir.Opts != "" {
			toAdd += ":" + dir.Opts
		}
		res = append(res, toAdd)
	}

	if c.InheritEnv {
		res = append(res, "--full-env")
	}
	for _, env := range c.EnvToInherit {
		res = append(res, "--env="+env)
	}

	if c.EnvToSet != nil {
		for key, val := range c.EnvToSet {
			res = append(res, "--env="+key+"="+val)
		}
	}

	if c.TimeLimit != 0 {
		res = append(res, "--time="+strconv.FormatFloat(c.TimeLimit, 'f', -1, 64))
	}
	if c.WallTimeLimit != 0 {
		res = append(res, "--wall-time="+strconv.FormatFloat(c.WallTimeLimit, 'f', -1, 64))
	}

	if c.MemoryLimit != 0 {
		res = append(res, "--cg-mem="+strconv.Itoa(c.MemoryLimit))
	}

	if c.InputPath == "" {
		c.InputPath = "/dev/null"
	}
	if c.OutputPath == "" {
		c.OutputPath = "/dev/null"
	}
	res = append(res, "--stdin="+c.InputPath)
	res = append(res, "--stdout="+c.OutputPath)

	if c.StderrToStdout {
		res = append(res, "--stderr-to-stdout")
	} else {
		if c.StderrPath == "" {
			c.StderrPath = "/dev/null"
		}
		res = append(res, "--stderr="+c.StderrPath)
	}

	if metaFile != nil {
		res = append(res, "--meta="+metaFile.Name())
	}

	res = append(res, "--silent", "--run", "--")

	return
}

// WriteFile writes an eval file to the specified path inside the box
func (b *IsolateBox) WriteFile(fpath string, r io.Reader, mode fs.FileMode) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return writeFile(b.getFilePath(fpath), r, mode)
}

func (b *IsolateBox) ReadFile(fpath string, w io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return readFile(b.getFilePath(fpath), w)
}

func (b *IsolateBox) GetID() int {
	return b.boxID
}

// FileExists returns if a file exists or not
func (b *IsolateBox) FileExists(fpath string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return checkFile(b.getFilePath(fpath))
}

// getFilePath returns a path to the file location on disk of a box file
func (b *IsolateBox) getFilePath(boxpath string) string {
	return path.Join(b.path, boxpath)
}

func (b *IsolateBox) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return exec.Command(IsolatePath, "--cg", "--box-id="+strconv.Itoa(b.boxID), "--cleanup").Run()
}

func (b *IsolateBox) runCommand(ctx context.Context, params []string, metaFile *os.File) (*RunStats, error) {
	var isolateOut bytes.Buffer
	cmd := exec.CommandContext(ctx, IsolatePath, params...)
	cmd.Stdout = &isolateOut
	cmd.Stderr = &isolateOut
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		return nil, err
	}

	// read Meta File
	defer metaFile.Close()
	defer os.Remove(metaFile.Name())
	return parseMetaFile(metaFile, isolateOut), nil
}

func (b *IsolateBox) RunCommand(ctx context.Context, command []string, conf *RunConfig) (*RunStats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var meta *RunStats

	if strings.HasPrefix(command[0], "/box") {
		p := b.getFilePath(command[0])
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				log.Printf("Executable does not exist in sandbox and will probably error in box %d", b.boxID)
			} else {
				log.Println(err)
			}
		}
	}

	for i := 1; i <= runErrRetries; i++ {

		metaFile, err := os.CreateTemp("", "sandbox-meta-*")
		if err != nil {
			log.Printf("Could not initialize temporary file: %v", err)
			continue
		}
		defer os.Remove(metaFile.Name())
		meta, err = b.runCommand(ctx, append(b.buildRunFlags(conf, metaFile), command...), metaFile)
		if err == nil && meta != nil && meta.Status != "XX" {
			if meta.ExitCode == 127 {
				if strings.Contains(meta.InternalMessage, "execve") { // It's text file busy, most likely...
					time.Sleep(runErrTimeout)
					continue
				}
			}
			return meta, err
		}

		if i > 1 {
			// Only warn if it comes to the second attempt. First error is often enough in prod
			log.Printf("Run error in box %d, retrying (%d/%d).", b.boxID, i, runErrRetries)
		}
		time.Sleep(runErrTimeout)
	}

	return meta, nil
}

// New returns a new box instance from the specified ID
func New(id int) (*IsolateBox, error) {
	ret, err := exec.Command(IsolatePath, "--cg", fmt.Sprintf("--box-id=%d", id), "--init").CombinedOutput()
	if strings.HasPrefix(string(ret), "Box already exists") {
		log.Println("Box reset: ", id)
		if out, err := exec.Command(IsolatePath, "--cg", fmt.Sprintf("--box-id=%d", id), "--cleanup").CombinedOutput(); err != nil {
			log.Println(err, string(out))
		}
		return New(id)
	}

	if strings.Contains(string(ret), "incompatible control group mode") { // Created without --cg
		log.Println("Box reset: ", id)
		if out, err := exec.Command(IsolatePath, fmt.Sprintf("--box-id=%d", id), "--cleanup").CombinedOutput(); err != nil {
			log.Println(err, string(out))
		}
		return New(id)
	}

	if strings.HasPrefix(string(ret), "Must be started as root") {
		if err := os.Chown(IsolatePath, 0, 0); err != nil {
			fmt.Println("Couldn't chown root the isolate binary:", err)
			return nil, err
		}
		return New(id)
	}

	if err != nil {
		return nil, err
	}

	return &IsolateBox{path: strings.TrimSpace(string(ret)), boxID: id}, nil
}

func IsolateVersion() string {
	ret, err := exec.Command(IsolatePath, "--version").CombinedOutput()
	if err != nil {
		return "precompiled?"
	}
	line, _, _ := bytes.Cut(ret, []byte{'\n'})
	return strings.TrimPrefix(string(line), "The process isolator ")
}

// parseMetaFile parses a specified meta file
func parseMetaFile(r io.Reader, out bytes.Buffer) *RunStats {
	if r == nil {
		return nil
	}
	var file = new(RunStats)

	file.InternalMessage = out.String()

	s := bufio.NewScanner(r)

	for s.Scan() {
		key, val, found := strings.Cut(s.Text(), ":")
		if !found {
			continue
		}
		switch key {
		case "cg-mem":
			file.Memory, _ = strconv.Atoi(val)
		case "exitcode":
			file.ExitCode, _ = strconv.Atoi(val)
		case "exitsig":
			file.ExitSignal, _ = strconv.Atoi(val)
		case "killed":
			file.Killed = true
		case "message":
			file.Message = val
		case "status":
			file.Status = val
		case "time":
			file.Time, _ = strconv.ParseFloat(val, 64)
		case "time-wall":
			// file.WallTime, _ = strconv.ParseFloat(val, 32)
			continue
		case "max-rss", "csw-voluntary", "csw-forced", "cg-enabled", "cg-oom-killed":
			continue
		default:
			log.Printf("Unknown isolate stat: %q (value: %v)", key, val)
			continue
		}
	}

	return file
}

type RunConfig struct {
	StderrToStdout bool

	InputPath string
	// If OutputPath or StderrPath are empty strings, they should default
	// to "/dev/null" for security reasons.
	OutputPath string
	StderrPath string

	MemoryLimit int

	TimeLimit     float64
	WallTimeLimit float64

	InheritEnv   bool
	EnvToInherit []string
	EnvToSet     map[string]string

	Directories []Directory
}

// Directory represents a directory rule
type Directory struct {
	In      string `toml:"in"`
	Out     string `toml:"out"`
	Opts    string `toml:"opts"`
	Removes bool   `toml:"removes"`

	// Verbatim doesn't set Out to In implicitly if it isn't set
	Verbatim bool `toml:"verbatim"`
}

type RunStats struct {
	Memory int `json:"memory"`

	ExitCode   int  `json:"exit_code"`
	ExitSignal int  `json:"exit_signal"`
	Killed     bool `json:"killed"`

	Message string `json:"message"`
	Status  string `json:"status"`

	Time float64 `json:"time"`

	InternalMessage string `json:"internal_msg"`
	// WallTime float64 `json:"wall_time"`
}

func readFile(p string, w io.Writer) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(w, f)
	return err
}

func writeFile(p string, r io.Reader, mode fs.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_SYNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err1 := f.Sync(); err1 != nil && err == nil {
		err = err1
	}
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}
	return err
}

func checkFile(p string) bool {
	_, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		log.Printf("File stat (%q) returned weird error: %s", p, err)
		return false
	}
	return true
}
