package main

// Checks all SGF files in this directory, and reports any Dyer Signature collisions.

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/fohristiwhirl/sgf"
)

func main() {

	if len(os.Args) < 2 {
		return
	}

	files, err := ioutil.ReadDir(os.Args[1])
	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	var dyers = make(map[string]string)			// dyer --> filename
	count := 0

	for _, file := range files {

		filename := file.Name()
		
		if strings.HasSuffix(filename, ".sgf") {

			full_path := filepath.Join(os.Args[1], filename)

			root, err := sgf.Load(full_path)
			if err != nil {
				fmt.Printf("%v\n", err)
			}

			dyer := root.Dyer()

			if already_seen_filename, ok := dyers[dyer]; ok {
				fmt.Printf("Collision:  %s  ==  %s\n", filename, already_seen_filename)
			} else {
				dyers[dyer] = filename
			}

			count++
		}
	}

	fmt.Printf("%d files checked.\n", count)
}
