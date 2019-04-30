package main

// Checks all SGF files in this directory, and reports any Dyer Signature collisions.

import (
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/fohristiwhirl/sgf"
)

func main() {
	var dyers = make(map[string]string)			// dyer --> filename
	files, _ := ioutil.ReadDir(".")
	count := 0

	for _, file := range files {
		filename := file.Name()
		if strings.HasSuffix(filename, ".sgf") {
			root, err := sgf.Load(filename)
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
