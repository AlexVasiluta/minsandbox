package main

import (
	"bytes"
	"context"
	"log"
	"strings"

	"github.com/AlexVasiluta/minsandbox/sandbox"
)

// NOTE: You need to run as sudo

func main() {
	if err := sandbox.Initialize(); err != nil {
		log.Fatal(err)
	}
	box, err := sandbox.New(50)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := box.Close(); err != nil {
			log.Println(err)
		}
	}()

	if err := box.WriteFile("/box/prog.in", strings.NewReader(`muie webshiti`), 0644); err != nil {
		log.Fatal(err)
	}

	if err := box.WriteFile("/box/main.py", strings.NewReader(`print(input())`), 0644); err != nil {
		log.Fatal(err)
	}

	cmd, err := sandbox.MakeGoodCommand([]string{"/usr/bin/python3", "/box/main.py"})
	if err != nil {
		log.Fatal(err)
	}
	// stats contains useful stuff like exit code/signal, memory/time limit, you can play around and see what you require
	stats, err := box.RunCommand(context.Background(), cmd, &sandbox.RunConfig{
		// Redirect stdout to this file
		OutputPath:     "/box/prog.out",
		StderrToStdout: true,
		// Memory limit in kbytes
		MemoryLimit: 1024 * 1024,
		// Time limit in seconds
		TimeLimit: 1.5,
		// Optional, you can write the standard input for the program in a file that can be written using box.WriteFile before running
		// Note that it will error out if you don't write it but specify it here
		InputPath: "/box/prog.in",
		// Usually a good practice to set it to something like a constatn * TimeLimit, since a long sleep() will not count towards the time limit
		// Uses a "wall clock" to limit the time allowed for the process to run
		WallTimeLimit: 1.5 * 2,
		// You can do this or specify custom env using EnvToSet
		InheritEnv: true,
		Directories: []sandbox.Directory{
			// Use https://github.com/KiloProjects/Kilonova/blob/master/eval/languages.go
			// for reference. In this case, it looks python requires /etc bound in the sandbox (read only)
			{In: "/etc"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	_ = stats
	var b bytes.Buffer
	if err := box.ReadFile("/box/prog.out", &b); err != nil {
		log.Fatal(err)
	}
	log.Printf("Got response from sandbox: %s", b.String())
}
