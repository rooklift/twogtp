package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fohristiwhirl/sgf"
)

type ConfigStruct struct {
	Engine1Name			string				`json:"engine_1_name"`
	Engine1Path			string				`json:"engine_1_path"`
	Engine1Args			[]string			`json:"engine_1_args"`
	Engine1Commands		[]string			`json:"engine_1_commands"`

	Engine2Name			string				`json:"engine_2_name"`
	Engine2Path			string				`json:"engine_2_path"`
	Engine2Args			[]string			`json:"engine_2_args"`
	Engine2Commands		[]string			`json:"engine_2_commands"`

	Timeout				time.Duration		`json:"timeout_seconds"`		// Note: at load, is multiplied by time.Second
}

var Config ConfigStruct
var KillTime = make(chan time.Time, 1024)	// Push back the timeout death of the app by sending to this.
var RegisterEngine = make(chan *Engine, 8)

func init() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s config_file\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	}
	file, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		panic("Couldn't load config file " + os.Args[1])
	}
	err = json.Unmarshal(file, &Config)
	if err != nil {
		panic("Couldn't parse JSON: " + err.Error())
	}

	Config.Timeout *= time.Second

	go killer()
}

type Engine struct {
	stdin		io.WriteCloser
	stdout		*bufio.Scanner
	stderr		*bufio.Scanner

	name		string			// For the SGF "PB" or "PW" properties
	dir			string			// Working directory
	base		string			// Command name, e.g. "leelaz.exe"

	args		[]string		// Not including base
	commands	[]string		// GTP commands to be sent at start, e.g. time limit

	wins		int
	losses		int
	unknowns	int

	process		*os.Process
}

func (self *Engine) Start(name, path string, args []string, commands []string) {

	self.name = name
	self.dir = filepath.Dir(path)
	self.base = filepath.Base(path)
	self.commands = commands

	for _, a := range args {
		self.args = append(self.args, a)
	}

	var cmd exec.Cmd

	cmd.Dir = self.dir
	cmd.Path = self.base
	cmd.Args = append([]string{self.base}, self.args...)

	var err1 error
	self.stdin, err1 = cmd.StdinPipe()

	stdout_pipe, err2 := cmd.StdoutPipe()
	self.stdout = bufio.NewScanner(stdout_pipe)

	stderr_pipe, err3 := cmd.StderrPipe()
	self.stderr = bufio.NewScanner(stderr_pipe)

	err4 := cmd.Start()

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		panic(fmt.Sprintf("\nerr1: %v\nerr2: %v\nerr3: %v\nerr4: %v\n", err1, err2, err3, err4))
	}

	self.process = cmd.Process

	RegisterEngine <- self			// Let the killer goroutine know we exist

	go self.ConsumeStderr()
}

func (self *Engine) ConsumeStderr() {
	for self.stderr.Scan() {
		// fmt.Printf("%s\n", self.stderr.Text())
	}
}

func (self *Engine) SendAndReceive(msg string) (string, error) {

	msg = strings.TrimSpace(msg)
	fmt.Fprintf(self.stdin, "%s\n", msg)

	var buf bytes.Buffer

	for self.stdout.Scan() {

		t := self.stdout.Text()
		if len(t) > 0 && buf.Len() > 0 {	// We got a meaningful line, and already had some, so add a \n between them.
			buf.WriteString("\n")
		}
		buf.WriteString(t)

		if len(t) == 0 {					// Last scan was an empty line, meaning the response has ended.

			s := strings.TrimSpace(buf.String())

			if len(s) == 0 {				// Didn't even get an =
				return "", fmt.Errorf("SendAndReceive(): got empty response")
			}

			if s[0] != '=' {
				return "", fmt.Errorf("SendAndReceive(): got reply: %s", strings.TrimSpace(s))
			}

			// Seems we got a sane response.
			// Return everything except the leading ID thing...

			i := 0

			for i < len(s) && (s[i] == '=' || s[i] >= '0' && s[i] <= '9') {
				i++
			}

			return strings.TrimSpace(s[i:]), nil
		}
	}

	// If we get to here, Scan() returned false, likely meaning the engine is dead.

	return "", fmt.Errorf("SendAndReceive(): %s crashed", self.name)
}

func main() {

	KillTime <- time.Now().Add(2 * time.Minute)		// 2 minute grace period to start up.

	a := new(Engine)
	b := new(Engine)
	a.Start(Config.Engine1Name, Config.Engine1Path, Config.Engine1Args, Config.Engine1Commands)
	b.Start(Config.Engine2Name, Config.Engine2Path, Config.Engine2Args, Config.Engine2Commands)

	engines := []*Engine{a, b}
	swap := false

	for {
		err := play_game(engines, swap)
		print_scores(engines)
		if err != nil {
			clean_quit(1, engines)
		}
		swap = !swap
	}
}

func play_game(engines []*Engine, swap bool) error {

	black := engines[0]
	white := engines[1]
	if swap {
		black, white = white, black
	}

	root := sgf.NewTree(19)
	root.SetValue("KM", "7.5")

	root.SetValue("C", fmt.Sprintf("Black:  %s\n%v\n\nWhite:  %s\n%v",
		black.base,
		black.args,
		white.base,
		white.args))

	root.SetValue("PB", black.name)
	root.SetValue("PW", white.name)

	for _, engine := range engines {
		engine.SendAndReceive("boardsize 19")
		engine.SendAndReceive("komi 7.5")
		engine.SendAndReceive("clear_board")

		for _, command := range engine.commands {
			engine.SendAndReceive(command)
		}
	}

	last_save_time := time.Now()
	passes_in_a_row := 0
	node := root

	outfilename := time.Now().Format("2006-01-02-15-04-05") + ".sgf"

	var final_error error

	colour := sgf.WHITE			// Swapped at start of loop...

	for {
		colour = colour.Opposite()

		var engine, opponent *Engine
		if colour == sgf.BLACK { engine, opponent = black, white }
		if colour == sgf.WHITE { engine, opponent = white, black }

		if time.Now().Sub(last_save_time) > 5 * time.Second {
			node.Save(outfilename)
			last_save_time = time.Now()
		}

		move, err := engine.SendAndReceive(fmt.Sprintf("genmove %s", colour.Lower()))

		fmt.Printf(move + " ")

		KillTime <- time.Now().Add(Config.Timeout)	// Delay the timeout death of this app.

		if err != nil {
			root.SetValue("RE", fmt.Sprintf("%s+F", colour.Opposite().Upper()))
			engine.losses++
			opponent.wins++
			final_error = err						// Set the error to return to caller.
			break
		} else if move == "resign" {
			root.SetValue("RE", fmt.Sprintf("%s+R", colour.Opposite().Upper()))
			engine.losses++
			opponent.wins++
			break
		} else if move == "pass" {
			passes_in_a_row++
			node = node.PassColour(colour)
			if passes_in_a_row >= 3 {
				engine.unknowns++
				opponent.unknowns++
				break
			}
		} else {
			passes_in_a_row = 0
			node, err = node.PlayMoveColour(sgf.ParseGTP(move, 19), colour)
			if err != nil {
				root.SetValue("RE", fmt.Sprintf("%s+F", colour.Opposite().Upper()))
				engine.losses++
				opponent.wins++
				final_error = err					// Set the error to return to caller.
				break
			}
		}

		// Relay the move. Must only get here with a valid move variable (including "pass")

		_, err = opponent.SendAndReceive(fmt.Sprintf("play %s %s", colour.Lower(), move))

		if err != nil {
			root.SetValue("RE", fmt.Sprintf("%s+F", colour.Upper()))
			engine.wins++
			opponent.losses++
			final_error = err
			break
		}
	}

	if final_error != nil {
		fmt.Printf("\n\n%v", final_error)
	}

	node.Save(outfilename)
	return final_error
}

func killer() {

	// Kill the app if we get past the most recent deadline sent to us.
	// This is NOT the only way the app can quit.

	var killtime time.Time
	var fts_armed bool				// Have we ever received an update?

	var engines []*Engine

	for {

		time.Sleep(642 * time.Millisecond)

		ClearChannels:
		for {
			select {
			case killtime = <- KillTime:
				fts_armed = true
			case engine := <- RegisterEngine:
				engines = append(engines, engine)
			default:
				break ClearChannels
			}
		}

		if fts_armed == false {
			continue
		}

		if time.Now().After(killtime) {
			fmt.Printf("killer(): timeout\n")
			clean_quit(1, engines)
		}
	}
}

func clean_quit(n int, engines []*Engine) {
	for _, engine := range engines {
		fmt.Printf("Killing %s...", engine.name)
		err := engine.process.Kill()
		if err != nil {
			fmt.Printf(" %v", err)
		}
		fmt.Printf("\n")
	}
	os.Exit(n)
}

func print_scores(engines []*Engine) {

	fmt.Printf("\n\n")
	fmt.Printf("%s: %d wins, %d losses", engines[0].name, engines[0].wins, engines[0].losses)
	if engines[0].unknowns > 0 {
		fmt.Printf(", %d unknown", engines[0].unknowns)
	}
	fmt.Printf("\n")
	fmt.Printf("%s: %d wins, %d losses", engines[1].name, engines[1].wins, engines[1].losses)
	if engines[1].unknowns > 0 {
		fmt.Printf(", %d unknown", engines[1].unknowns)
	}
	fmt.Printf("\n\n")
}
