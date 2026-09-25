package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/kuasar-sandbox/connector/pkg/debug"
	"golang.org/x/sys/unix"
)

// selectDebugSwitch discovers pinned switches without changing their state.
func selectDebugSwitch() (string, error) {
	entries, err := os.ReadDir("/sys/fs/bpf")
	if err != nil {
		return "", err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		sw, err := debug.OpenSwitch(name)
		if err != nil {
			continue
		}
		if sw.Config() != nil {
			names = append(names, name)
		}
		_ = sw.Close()
	}
	sort.Strings(names)
	return chooseDebugSwitch(names, termIsTTY(int(os.Stdin.Fd())), os.Stdin)
}

func chooseDebugSwitch(names []string, interactive bool, input io.Reader) (string, error) {
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no switches found; specify a switch name")
	case 1:
		fmt.Fprintf(os.Stderr, "debug: using switch %s\n", names[0])
		return names[0], nil
	}
	if !interactive {
		return "", fmt.Errorf("multiple switches found (%s); specify one", strings.Join(names, ", "))
	}
	fmt.Fprintln(os.Stderr, "Select a switch:")
	for i, name := range names {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, name)
	}
	fmt.Fprint(os.Stderr, "Selection: ")
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return "", err
	}
	i, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || i < 1 || i > len(names) {
		return "", fmt.Errorf("invalid switch selection")
	}
	return names[i-1], nil
}

func termIsTTY(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}
