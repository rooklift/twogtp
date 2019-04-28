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

// Some use of globals, because reasons...

var Config ConfigStruct
var KillTime = make(chan time.Time, 1024)	// Push back the timeout death of the app by sending to this.
var Engines map[sgf.Colour]*Engine

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

	var response bytes.Buffer
	for self.stdout.Scan() {

		response.WriteString(self.stdout.Text())
		response.WriteString("\n")

		if self.stdout.Text() == "" {

			// Return everything except the leading ID thing...

			s := response.String()
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

	Engines = map[sgf.Colour]*Engine{sgf.BLACK: a, sgf.WHITE: b}

	for {
		err := play_game()
		if err != nil {
			clean_quit(1)
		}
		Engines[sgf.WHITE], Engines[sgf.BLACK] = Engines[sgf.BLACK], Engines[sgf.WHITE]
	}
}

func play_game() error {

	root := sgf.NewTree(19)
	root.SetValue("KM", "7.5")

	root.SetValue("C", fmt.Sprintf("Black:  %s\n%v\n\nWhite:  %s\n%v",
		Engines[sgf.BLACK].base,
		Engines[sgf.BLACK].args,
		Engines[sgf.WHITE].base,
		Engines[sgf.WHITE].args))

	root.SetValue("PB", Engines[sgf.BLACK].name)
	root.SetValue("PW", Engines[sgf.WHITE].name)

	for _, engine := range Engines {
		engine.SendAndReceive("boardsize 19")
		engine.SendAndReceive("komi 7.5")
		engine.SendAndReceive("clear_board")

		for _, command := range engine.commands {
			engine.SendAndReceive(command)
		}
	}

	last_save_time := time.Now()
	colour := sgf.WHITE
	node := root

	passes_in_a_row := 0

	outfilename := time.Now().Format("2006-01-02-15-04-05") + ".sgf"

	var final_error error

	for {
		colour = colour.Opposite()

		if time.Now().Sub(last_save_time) > 5 * time.Second {
			node.Save(outfilename)
			last_save_time = time.Now()
		}

		move, err := Engines[colour].SendAndReceive(fmt.Sprintf("genmove %s", colour.Lower()))

		fmt.Printf(move + " ")

		KillTime <- time.Now().Add(Config.Timeout)	// Delay the timeout death of this app.

		if err != nil {
			root.SetValue("RE", fmt.Sprintf("%s+F", colour.Opposite().Upper()))
			Engines[colour].losses++
			Engines[colour.Opposite()].wins++
			final_error = err						// Set the error to return to caller.
			break
		} else if move == "resign" {
			root.SetValue("RE", fmt.Sprintf("%s+R", colour.Opposite().Upper()))
			Engines[colour].losses++
			Engines[colour.Opposite()].wins++
			break
		} else if move == "pass" {
			passes_in_a_row++
			node = node.PassColour(colour)
			if passes_in_a_row >= 3 {
				Engines[colour].unknowns++
				Engines[colour.Opposite()].unknowns++
				break
			}
		} else {
			passes_in_a_row = 0
			node, err = node.PlayMoveColour(sgf.ParseGTP(move, 19), colour)
			if err != nil {
				root.SetValue("RE", fmt.Sprintf("%s+F", colour.Opposite().Upper()))
				Engines[colour].losses++
				Engines[colour.Opposite()].wins++
				final_error = err					// Set the error to return to caller.
				break
			}
		}

		// Must only get here with a valid move variable (including "pass")

		other_engine := Engines[colour.Opposite()]
		other_engine.SendAndReceive(fmt.Sprintf("play %s %s", colour.Lower(), move))
	}

	if final_error != nil {
		fmt.Printf("\n\n%v", final_error)
	}

	node.Save(outfilename)

	fmt.Printf("\n\n")
	fmt.Printf("%s: %d wins, %d losses\n", Engines[sgf.BLACK].name, Engines[sgf.BLACK].wins, Engines[sgf.BLACK].losses)
	fmt.Printf("%s: %d wins, %d losses\n", Engines[sgf.WHITE].name, Engines[sgf.WHITE].wins, Engines[sgf.WHITE].losses)
	fmt.Printf("\n")

	return final_error
}

func killer() {

	// Kill the app if we get past the most recent deadline sent to us.
	// This is NOT the only way the app can quit.

	var killtime time.Time
	var fts_armed bool				// Have we ever received an update?

	for {

		time.Sleep(642 * time.Millisecond)

		ClearChannel:
		for {
			select {
			case killtime = <- KillTime:
				fts_armed = true
			default:
				break ClearChannel
			}
		}

		if fts_armed == false {
			continue
		}

		if time.Now().After(killtime) {
			fmt.Printf("killer(): timeout\n")
			clean_quit(1)
		}
	}
}

func clean_quit(n int) {
	for _, engine := range Engines {
		fmt.Printf("Killing %s... ", engine.name)
		err := engine.process.Kill()
		fmt.Printf("%v\n", err)
	}
	os.Exit(n)
}
