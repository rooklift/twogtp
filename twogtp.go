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
	"strconv"
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
	PassingWins			bool				`json:"passing_wins"`			// Surprisingly good heuristic for LZ at least
	Restart				bool				`json:"restart"`
	Games				int					`json:"games"`
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
	Stdin		io.WriteCloser
	Stdout		*bufio.Scanner

	Name		string			// For the SGF "PB" or "PW" properties
	Dir			string			// Working directory
	Base		string			// Command name, e.g. "leelaz.exe"

	Args		[]string		// Not including base
	Commands	[]string		// GTP commands to be sent at start, e.g. time limit

	Process		*os.Process

	wins_b		int
	losses_b	int
	wins_w		int
	losses_w	int
}

func (self *Engine) Start(name, path string, args []string, commands []string) {

	self.Name = name
	self.Dir = filepath.Dir(path)
	self.Base = filepath.Base(path)

	self.Args = make([]string, len(args))
	copy(self.Args, args)

	self.Commands = make([]string, len(commands))
	copy(self.Commands, commands)

	self.Restart()

	RegisterEngine <- self			// Let the killer goroutine know we exist
}

func (self *Engine) Restart() {

	if self.Process != nil {
		self.Process.Kill()
	}

	var cmd exec.Cmd

	cmd.Dir = self.Dir
	cmd.Path = self.Base
	cmd.Args = append([]string{self.Base}, self.Args...)

	var err1 error
	self.Stdin, err1 = cmd.StdinPipe()

	stdout_pipe, err2 := cmd.StdoutPipe()
	self.Stdout = bufio.NewScanner(stdout_pipe)

	stderr_pipe, err3 := cmd.StderrPipe()
	stderr := bufio.NewScanner(stderr_pipe)

	err4 := cmd.Start()

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		panic(fmt.Sprintf("\nerr1: %v\nerr2: %v\nerr3: %v\nerr4: %v\n", err1, err2, err3, err4))
	}

	self.Process = cmd.Process

	go consume_scanner(stderr)
}

func consume_scanner(scanner *bufio.Scanner) {
	for scanner.Scan() {								// Will end when the pipe closes due to an engine restart.
		// fmt.Printf("%s\n", self.stderr.Text())
	}
}

func (self *Engine) ScoreElements() []string {

	// name, wins, win%, black_wins, black_win%, white_wins, white_win%

	var ret []string

	wins    := self.wins_b   + self.wins_w
	losses  := self.losses_b + self.losses_w
	games   := wins          + losses					// FIXME if we ever have unknowns.
	games_b := self.wins_b   + self.losses_b
	games_w := self.wins_w   + self.losses_w

	ret = append(ret, self.Name)

	ret = append(ret, strconv.Itoa(wins))
	if games > 0 {
		ret = append(ret, fmt.Sprintf("%.0f%%", 100.0 * float64(wins) / float64(games)))
	} else {
		ret = append(ret, "0%")
	}

	ret = append(ret, strconv.Itoa(self.wins_b))
	if games_b > 0 {
		ret = append(ret, fmt.Sprintf("%.0f%%", 100.0 * float64(self.wins_b) / float64(games_b)))
	} else {
		ret = append(ret, "0%")
	}

	ret = append(ret, strconv.Itoa(self.wins_w))
	if games_w > 0 {
		ret = append(ret, fmt.Sprintf("%.0f%%", 100.0 * float64(self.wins_w) / float64(games_w)))
	} else {
		ret = append(ret, "0%")
	}

	return ret
}

func (self *Engine) Win(colour sgf.Colour) {
	if colour == sgf.BLACK {
		self.wins_b++
	} else if colour == sgf.WHITE {
		self.wins_w++
	} else {
		panic("bad colour")
	}
}

func (self *Engine) Lose(colour sgf.Colour) {
	if colour == sgf.BLACK {
		self.losses_b++
	} else if colour == sgf.WHITE {
		self.losses_w++
	} else {
		panic("bad colour")
	}
}

func (self *Engine) SendAndReceive(msg string) (string, error) {

	msg = strings.TrimSpace(msg)
	fmt.Fprintf(self.Stdin, "%s\n", msg)

	var buf bytes.Buffer

	for self.Stdout.Scan() {

		t := self.Stdout.Text()
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

	return "", fmt.Errorf("SendAndReceive(): %s crashed", self.Name)
}

func main() {

	KillTime <- time.Now().Add(2 * time.Minute)		// 2 minute grace period to start up.

	a := new(Engine)
	b := new(Engine)
	a.Start(Config.Engine1Name, Config.Engine1Path, Config.Engine1Args, Config.Engine1Commands)
	b.Start(Config.Engine2Name, Config.Engine2Path, Config.Engine2Args, Config.Engine2Commands)

	engines := []*Engine{a, b}
	swap := false

	for n := 0; n < Config.Games; n++ {
		err := play_game(engines, swap)
		print_scores(engines)
		if err != nil {
			clean_quit(1, engines)
		}
		if Config.Restart {
			engines[0].Restart()
			engines[1].Restart()
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
		black.Base,
		black.Args,
		white.Base,
		white.Args))

	root.SetValue("PB", black.Name)
	root.SetValue("PW", white.Name)

	for _, engine := range engines {
		engine.SendAndReceive("boardsize 19")
		engine.SendAndReceive("komi 7.5")
		engine.SendAndReceive("clear_board")
		engine.SendAndReceive("clear_cache")		// Always wanted where available

		for _, command := range engine.Commands {
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
			engine.Lose(colour)
			opponent.Win(colour.Opposite())
			final_error = err						// Set the error to return to caller. This kills the app.
			break
		} else if move == "resign" {
			root.SetValue("RE", fmt.Sprintf("%s+R", colour.Opposite().Upper()))
			engine.Lose(colour)
			opponent.Win(colour.Opposite())
			break
		} else if move == "pass" {
			passes_in_a_row++
			node = node.PassColour(colour)
			if Config.PassingWins {
				root.SetValue("RE", fmt.Sprintf("%s+", colour.Upper()))
				node.SetValue("C", fmt.Sprintf("%s declared victory.", engine.Base))
				engine.Win(colour)
				opponent.Lose(colour.Opposite())
				break
			}
			if passes_in_a_row >= 2 {
				// FIXME: get the result somehow...
				break
			}
		} else {
			passes_in_a_row = 0
			node, err = node.PlayMoveColour(sgf.ParseGTP(move, 19), colour)
			if err != nil {
				root.SetValue("RE", fmt.Sprintf("%s+F", colour.Opposite().Upper()))
				engine.Lose(colour)
				opponent.Win(colour.Opposite())
				final_error = err					// Set the error to return to caller. This kills the app.
				break
			}
		}

		// Relay the move. Must only get here with a valid move variable (including "pass")

		_, err = opponent.SendAndReceive(fmt.Sprintf("play %s %s", colour.Lower(), move))

		if err != nil {
			root.SetValue("RE", fmt.Sprintf("%s+F", colour.Upper()))
			engine.Win(colour)
			opponent.Lose(colour.Opposite())
			final_error = err						// Set the error to return to caller. This kills the app.
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
		fmt.Printf("Killing %s...", engine.Name)
		err := engine.Process.Kill()
		if err != nil {
			fmt.Printf(" %v", err)
		}
		fmt.Printf("\n")
	}
	os.Exit(n)
}

func print_scores(engines []*Engine) {

	format := " %-20.20s   %4v %-7v %4v %-7v %4v %-7v\n"

	fmt.Printf("\n\n")
	fmt.Printf(format, "", "", "wins", "", "black", "", "white")
	for _, engine := range engines {
		elements := engine.ScoreElements()
		fmt.Printf(format, elements[0], elements[1], elements[2], elements[3], elements[4], elements[5], elements[6])
	}
	fmt.Printf("\n")
}
