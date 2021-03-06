package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const NAME = "pstree_prof"

type proc struct {
	User     string `json:"user"`
	Pid      int    `json:"pid"`
	Ppid     int    `json:"ppid"`
	Pgid     int    `json:"pgid"`
	Command  string `json:"command"`
	Children []int  `json:"children"`
}

type sample struct {
	At    time.Time    `json:"at"`
	Procs map[int]proc `json:"procs"`
}

func main() {
	command := flag.String("cmd", "", "Command to run")
	outputFmt := flag.String("fmt", "count", "Output format to summarize samples")
	freq := flag.Int("freq", 100, "Sampling frequency in Hertz")
	flag.Parse()

	if *command == "" {
		flag.Usage()
		log.Fatalln("a non-empty command must be specified")
	}

	log.SetPrefix(fmt.Sprintf("%s: ", NAME))

	delay := 1000 / *freq
	log.Printf("sampling every %dms\n", delay)
	delayMS := time.Duration(delay) * time.Millisecond

	samples := make([]sample, 1)
	commandParts := strings.Split(*command, " ")
	cmd, err := startCommandInBackground(commandParts[0], commandParts[1:], func() {
		switch *outputFmt {
		case "count":
			printProcCounts(samples)
		case "starts_and_ends":
			printProcStartsAndEnds(samples)
		case "trace":
			exportSamplesAsTraces(samples)
		default:
			log.Fatalf("unrecognized outputMode: %s\n", *outputFmt)
		}
		os.Exit(0) // why do I need to do this?
	})
	if err != nil {
		log.Fatalln(err)
	}

	var lastSample sample
	for {
		lastSample = sampleProcs(cmd.Process.Pid, lastSample)
		samples = append(samples, lastSample)
		time.Sleep(delayMS)
	}
}

func sampleProcs(pid int, lastSample sample) sample {
	cols := []string{"user", "pid", "ppid", "pgid", "command"}
	args := []string{"ps", "-axwwo", strings.Join(cols, ",")}
	psCmd := exec.Command(args[0], args[1:]...)
	psOut, err := psCmd.Output()
	if err != nil {
		log.Fatalln(fmt.Errorf("could not start `ps`: %s", err))
	}

	lines := strings.Split(string(psOut), "\n")
	if len(lines) == 0 {
		log.Fatalln("expected at least one line of output from `ps`")
	}

	// skip header
	lines = lines[1:]
	// if last line is empty, skip
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	procs := make(map[int]proc)
	for _, line := range lines {
		proc := parseLineAsProc(line, cols)

		if proc.Pid == psCmd.Process.Pid {
			// not interested in the `ps ...` command that we started
			continue
		}

		procs[proc.Pid] = proc
	}

	for pid, proc := range procs {
		if parent, ok := procs[proc.Ppid]; ok {
			parent.Children = append(parent.Children, pid)
			procs[proc.Ppid] = parent
		}
	}

	type pidToVisit struct {
		pid, depth int
	}
	pidsToVisit := []pidToVisit{
		{pid, 0},
	}

	sample := sample{At: time.Now(), Procs: make(map[int]proc)}
	for len(pidsToVisit) > 0 {
		pid := pidsToVisit[0]
		pidsToVisit = pidsToVisit[1:]
		if _, ok := sample.Procs[pid.pid]; ok {
			continue
		}
		proc := procs[pid.pid]
		sample.Procs[pid.pid] = proc

		newPidsToVisit := make([]pidToVisit, len(proc.Children))
		for i := 0; i < len(proc.Children); i += 1 {
			newPidsToVisit[i] = pidToVisit{pid: proc.Children[i], depth: pid.depth + 1}
		}
		// append the new PIDs so they're visited first
		pidsToVisit = append(newPidsToVisit, pidsToVisit...)
	}

	return sample
}

func parseLineAsProc(line string, cols []string) proc {
	var colStart, col int
	prevWasSpace := false
	parsedCols := make([]string, len(cols))
	for i, c := range line {
		if col == len(cols)-1 {
			// final column, don't need to search for the end
			// abc___def___ghi
			//    	       ^
			parsedCols[col] = line[i:]
			break
		}

		if !prevWasSpace && c == ' ' {
			// first space char after a string of non-spaces, i.e. the start of the column padding
			// abc___def___ghi
			//    ^
			parsedCols[col] = line[colStart:i]
			col += 1
			prevWasSpace = true
		} else if prevWasSpace && c != ' ' {
			// first non-space after a string of spaces, i.e. the start of a new column
			// abc___def___ghi
			//       ^
			colStart = i
			prevWasSpace = false
		}
	}

	return proc{
		User:    parsedCols[0],
		Pid:     strictAtoi(parsedCols[1]),
		Ppid:    strictAtoi(parsedCols[2]),
		Pgid:    strictAtoi(parsedCols[3]),
		Command: parsedCols[4],
	}
}

func printProcCounts(samples []sample) {
	type countAndCommand struct {
		count int
		cmd   string
	}
	counts := make(map[int]countAndCommand)
	for _, sample := range samples {
		for _, proc := range sample.Procs {
			if cc, ok := counts[proc.Pid]; ok {
				counts[proc.Pid] = countAndCommand{count: cc.count + 1, cmd: cc.cmd}
			} else {
				counts[proc.Pid] = countAndCommand{count: 1, cmd: proc.Command}
			}
		}
	}

	countsAndCommands := make([]countAndCommand, len(counts))
	for _, count := range counts {
		countsAndCommands = append(countsAndCommands, count)
	}

	sort.SliceStable(countsAndCommands, func(i, j int) bool {
		return countsAndCommands[i].count > countsAndCommands[j].count
	})

	fmt.Println("count\tcommand")
	for _, cAndC := range countsAndCommands {
		if cAndC.count == 0 {
			continue
		}
		fmt.Printf("%d\t%s\n", cAndC.count, cAndC.cmd)
	}
}

func printProcStartsAndEnds(samples []sample) {
	fmt.Fprintf(os.Stderr, "event\tpid\tsample\tcmd\n")
	row := func(event string, pid int, nthSample int, cmd string) {
		fmt.Fprintf(os.Stderr, "%s\t%d\t%d\t%s\n", event, pid, nthSample, cmd)
	}

	procs := make(map[int]proc)
	for i, sample := range samples {
		for _, p := range sample.Procs {
			_, seenBefore := procs[p.Pid]
			if !seenBefore {
				row("started", p.Pid, i, p.Command)
				procs[p.Pid] = p
			}
		}
		for _, p := range procs {
			_, procStillRunning := sample.Procs[p.Pid]
			if !procStillRunning || i == len(samples)-1 {
				row("ended", p.Pid, i, p.Command)
				delete(procs, p.Pid)
			}
		}
	}
}

func startCommandInBackground(name string, args []string, afterCommand func()) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	log.Println("start of output from command:")
	err := cmd.Start()
	go func() {
		cmd.Wait()
		log.Println("end of output from command")
		afterCommand()
	}()
	if err != nil {
		return nil, fmt.Errorf("failed to start command: %s", err)
	}
	return cmd, nil
}

func strictAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
}
