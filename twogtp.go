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

type EngineConfig struct {
	Name				string				`json:"name"`
	Path				string				`json:"path"`
	Args				[]string			`json:"args"`
	Commands			[]string			`json:"commands"`
}

type ConfigStruct struct {
	EngineCfg			[]*EngineConfig		`json:"engines"`

	TimeoutSecs			time.Duration		`json:"timeout_seconds"`
	PassingWins			bool				`json:"passing_wins"`			// Surprisingly good heuristic for LZ at least
	Restart				bool				`json:"restart"`
	Games				int					`json:"games"`

	Size				int					`json:"size"`
	Komi				float64				`json:"komi"`

	Winners				string				`json:"winners"`
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
}

// -----------------------------------------------------------------------------

var config ConfigStruct

var KillTime = make(chan time.Time, 1024)	// Push back the timeout death of the app by sending to this.
var RegisterEngine = make(chan *Engine, 8)

// -----------------------------------------------------------------------------

func init() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s config_file\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	}

	d, f := filepath.Split(os.Args[1])
	if d == "" {
		d = "."
	}

	err := os.Chdir(d)
	if err != nil {
		fmt.Printf("Couldn't change working directory: %v\n", err)
		os.Exit(1)
	}

	file, err := ioutil.ReadFile(f)
	if err != nil {
		fmt.Printf("Couldn't load config file: %v\n", err)
		os.Exit(1)
	}

	err = json.Unmarshal(file, &config)
	if err != nil {
		fmt.Printf("Couldn't parse JSON: %v\n", err)
		os.Exit(1)
	}

	if config.Size < 1 {
		config.Size = 19
	} else if config.Size > 25 {
		fmt.Printf("Size %d not supported\n", config.Size)
		os.Exit(1)
	}

	if len(config.EngineCfg) != 2 {
		fmt.Printf("Expected 2 engines, got %d\n", len(config.EngineCfg))
		os.Exit(1)
	}

	if len(config.Winners) >= config.Games {
		fmt.Printf("\nMatch already ended. To play on, delete the winners field from the config file, or increase the games count.\n\n")
		config.PrintScores()
		os.Exit(0)
	}

	go killer()
}

func main() {

	KillTime <- time.Now().Add(2 * time.Minute)		// 2 minute grace period to start up.

	engines := []*Engine{new(Engine), new(Engine)}

	for n, e := range engines {
		e.Start(config.EngineCfg[n].Name, config.EngineCfg[n].Path, config.EngineCfg[n].Args, config.EngineCfg[n].Commands)
		if _, err := e.SendAndReceive("name"); err != nil {
			fmt.Printf("%v\n", err)
			clean_quit(1, engines)
		}
	}

	dyers := make(map[string]string)				// dyer --> first filename
	collisions := 0

	if len(config.Winners) > 0 {
		fmt.Printf("\n")
		config.PrintScores()
	}

	_, config_base := filepath.Split(os.Args[1])	// Earlier we did Chdir() to config's dir, so only need base

	for round := len(config.Winners); round < config.Games; round++ {

		root, filename, err := play_game(engines, round)

		config.Save(config_base)					// Save the scores

		new_dyer := root.Dyer()

		first_filename, exists := dyers[new_dyer]
		if exists {
			fmt.Printf("Game was similar to %s\n\n", first_filename)
			collisions++
		} else {
			dyers[new_dyer] = filename
		}

		config.PrintScores()

		if err != nil {
			clean_quit(1, engines)
		}

		if config.Restart {
			engines[0].Restart()
			engines[1].Restart()
		}
	}

	fmt.Printf("%d Dyer collisions noted.\n\n", collisions)

	clean_quit(0, engines)
}

func play_game(engines []*Engine, round int) (*sgf.Node, string, error) {

	var black_engine, white_engine *Engine

	if round % 2 == 0 {
		black_engine, white_engine = engines[0], engines[1]
	} else {
		black_engine, white_engine = engines[1], engines[0]
	}

	root := sgf.NewTree(config.Size)
	root.SetValue("KM", fmt.Sprintf("%.1f", config.Komi))

	root.SetValue("C", fmt.Sprintf("Black:  %s\n%v\n\nWhite:  %s\n%v",
		black_engine.Base,
		black_engine.Args,
		white_engine.Base,
		white_engine.Args))

	root.SetValue("PB", black_engine.Name)
	root.SetValue("PW", white_engine.Name)

	for _, e := range engines {
		e.SendAndReceive(fmt.Sprintf("boardsize %d", config.Size))
		e.SendAndReceive(fmt.Sprintf("komi %.1f", config.Komi))
		e.SendAndReceive("clear_board")
		e.SendAndReceive("clear_cache")		// Always wanted where available

		for _, command := range e.Commands {
			e.SendAndReceive(command)
		}
	}

	last_save_time := time.Now()
	passes_in_a_row := 0
	node := root

	var final_error error

	for turn := 0; true; turn++ {

		var colour sgf.Colour
		var engine, opponent *Engine

		if turn % 2 == 0 {
			colour = sgf.BLACK
			engine, opponent = black_engine, white_engine
		} else {
			colour = sgf.WHITE
			engine, opponent = white_engine, black_engine
		}

		if time.Now().Sub(last_save_time) > 5 * time.Second {
			node.Save("current.sgf")
			last_save_time = time.Now()
		}

		move, err := engine.SendAndReceive(fmt.Sprintf("genmove %s", colour.Lower()))

		fmt.Printf(move + " ")

		KillTime <- time.Now().Add(config.TimeoutSecs * time.Second)	// Delay the timeout death of this app.

		if err != nil {

			re := fmt.Sprintf("Void")
			config.Win(re)
			root.SetValue("RE", re)
			fmt.Printf(re)

			final_error = err						// Set the error to return to caller. This kills the app.
			break

		} else if move == "resign" {

			re := fmt.Sprintf("%s+R", colour.Opposite().Upper())
			config.Win(re)
			root.SetValue("RE", re)
			fmt.Printf(re)

			break

		} else if move == "pass" {

			passes_in_a_row++
			node = node.PassColour(colour)

			if config.PassingWins {

				re := fmt.Sprintf("%s+", colour.Upper())
				config.Win(re)
				root.SetValue("RE", re)
				fmt.Printf(re)

				node.SetValue("C", fmt.Sprintf("%s declared victory.", engine.Name))
				break
			}

			if passes_in_a_row >= 2 {
				// FIXME: get the result somehow...
				config.Win("")
				break
			}

		} else {

			passes_in_a_row = 0
			node, err = node.PlayColour(sgf.ParseGTP(move, config.Size), colour)

			if err != nil {

				re := fmt.Sprintf("Void")
				config.Win(re)
				root.SetValue("RE", re)
				fmt.Printf(re)

				final_error = err					// Set the error to return to caller. This kills the app.
				break
			}
		}

		// Relay the move. Must only get here with a valid move variable (including "pass")

		_, err = opponent.SendAndReceive(fmt.Sprintf("play %s %s", colour.Lower(), move))

		if err != nil {

			re := fmt.Sprintf("Void")
			config.Win(re)
			root.SetValue("RE", re)
			fmt.Printf(re)

			final_error = err						// Set the error to return to caller. This kills the app.
			break
		}
	}

	fmt.Printf("\n\n")

	if final_error != nil {
		fmt.Printf("%v\n\n", final_error)
	}

	outfilename := time.Now().Format("20060102-15-04-05") + ".sgf"
	for appendix := byte('a'); appendix <= 'z'; appendix++ {
		_, err := os.Stat(outfilename)
		if err == nil {					// File exists...
			outfilename = time.Now().Format("20060102-15-04-05") + string([]byte{appendix}) + ".sgf"
		} else {
			break
		}
	}

	node.Save(outfilename)
	os.Remove("current.sgf")

	return node.GetRoot(), outfilename, final_error
}

// -----------------------------------------------------------------------------

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
			fmt.Printf("\n\nkiller(): timeout\n")
			clean_quit(1, engines)
		}
	}
}

func clean_quit(n int, engines []*Engine) {
	for _, engine := range engines {
		if engine != nil && engine.Process != nil {
			fmt.Printf("Killing %s...", engine.Name)
			err := engine.Process.Kill()
			if err != nil {
				fmt.Printf(" %v", err)
			}
			fmt.Printf("\n")
		}
	}
	os.Exit(n)
}

// ---------------------------------------------------------------------------------------------

func (self *ConfigStruct) Win(re string) {

	// re is something like "B+R"

	if len(re) == 0 || (re[0] != 'B' && re[0] != 'W') {
		self.Winners += "0"								// Draw / unknown result
		return
	}

	if len(self.Winners) % 2 == 0 {						// Engine 1 is black
		if re[0] == 'B' {
			self.Winners += "1"
		} else {
			self.Winners += "2"
		}
	} else {											// Engine 2 is black
		if re[0] == 'B' {
			self.Winners += "2"
		} else {
			self.Winners += "1"
		}
	}
}

func (self *ConfigStruct) Save(filename string) {

	outfile, err := os.Create(filename)
	if err != nil {
		fmt.Printf("\n%v\n", err)
		return
	}
	defer outfile.Close()

	enc := json.NewEncoder(outfile)
	enc.SetIndent("", "\t")

	err = enc.Encode(self)
	if err != nil {
		fmt.Printf("\n%v\n", err)
		return
	}
}

func (self *ConfigStruct) PrintScores() {

	// name, wins, win%, black_wins, black_win%, white_wins, white_win%

	wins_1 := strings.Count(self.Winners, "1")
	wins_2 := strings.Count(self.Winners, "2")

	var winrate_1, winrate_2 float64

	valid_games := len(self.Winners) - strings.Count(self.Winners, "0")

	if valid_games > 0 {
		winrate_1 = float64(wins_1) / float64(valid_games)
		winrate_2 = float64(wins_2) / float64(valid_games)
	}

	black_wins_1 := 0
	white_wins_1 := 0
	black_wins_2 := 0
	white_wins_2 := 0

	for n := 0; n < len(self.Winners); n++ {
		if self.Winners[n] == '1' {
			if n % 2 == 0 {					// Engine 1 black, engine 2 white
				black_wins_1++
			} else {
				white_wins_1++
			}
		} else if self.Winners[n] == '2' {
			if n % 2 == 0 {					// Engine 1 black, engine 2 white, as above (same condition)
				white_wins_2++
			} else {
				black_wins_2++
			}
		}
	}

	var black_winrate_1, black_winrate_2, white_winrate_1, white_winrate_2 float64

	if black_wins_1 + white_wins_2 > 0 {
		black_winrate_1 = float64(black_wins_1) / float64(black_wins_1 + white_wins_2)
		white_winrate_2 = float64(white_wins_2) / float64(black_wins_1 + white_wins_2)
	}

	if black_wins_2 + white_wins_1 > 0 {
		black_winrate_2 = float64(black_wins_2) / float64(black_wins_2 + white_wins_1)
		white_winrate_1 = float64(white_wins_1) / float64(black_wins_2 + white_wins_1)
	}

	format1 := "%-20.20s   %4v %-7v %4v %-7v %4v %-7v\n"
	format2 := "%-20.20s   %4v %-7.2f %4v %-7.2f %4v %-7.2f\n"

	fmt.Printf(format1, "", "", "wins", "", "black", "", "white")
	fmt.Printf(format2, self.EngineCfg[0].Name, wins_1, winrate_1, black_wins_1, black_winrate_1, white_wins_1, white_winrate_1)
	fmt.Printf(format2, self.EngineCfg[1].Name, wins_2, winrate_2, black_wins_2, black_winrate_2, white_wins_2, white_winrate_2)
	fmt.Printf("\n")
}

// ---------------------------------------------------------------------------------------------

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

func consume_scanner(scanner *bufio.Scanner) {
	for scanner.Scan() {								// Will end when the pipe closes due to an engine restart.
		// fmt.Printf("%s\n", scanner.Text())
	}
}