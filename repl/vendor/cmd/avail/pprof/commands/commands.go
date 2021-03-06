// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package commands defines and manages the basic pprof commands
package commands

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"cmd/avail/pprof/plugin"
	"cmd/avail/pprof/report"
	"cmd/avail/pprof/svg"
	"cmd/avail/pprof/tempfile"
)

// Commands describes the commands accepted by pprof.
type Commands map[string]*Command

// Command describes the actions for a pprof command. Includes a
// function for command-line completion, the report format to use
// during report generation, any postprocessing functions, and whether
// the command expects a regexp parameter (typically a function name).
type Command struct {
	Complete    Completer     // autocomplete for interactive mode
	Format      int           // report format to generate
	PostProcess PostProcessor // postprocessing to run on report
	HasParam    bool          // Collect a parameter from the CLI
	Usage       string        // Help text
}

// Completer is a function for command-line autocompletion
type Completer func(prefix string) string

// PostProcessor is a function that applies post-processing to the report output
type PostProcessor func(input *bytes.Buffer, output io.Writer, ui plugin.UI) error

// PProf returns the basic pprof report-generation commands
func PProf(c Completer, interactive **bool) Commands {
	return Commands{
		// Commands that require no post-processing.
		"tags":   {nil, report.Tags, nil, false, "Outputs all tags in the profile"},
		"raw":    {c, report.Raw, nil, false, "Outputs a text representation of the raw profile"},
		"dot":    {c, report.Dot, nil, false, "Outputs a graph in DOT format"},
		"top":    {c, report.Text, nil, false, "Outputs top entries in text form"},
		"tree":   {c, report.Tree, nil, false, "Outputs a text rendering of call graph"},
		"text":   {c, report.Text, nil, false, "Outputs top entries in text form"},
		"disasm": {c, report.Dis, nil, true, "Output annotated assembly for functions matching regexp or address"},
		"list":   {c, report.List, nil, true, "Output annotated source for functions matching regexp"},
		"peek":   {c, report.Tree, nil, true, "Output callers/callees of functions matching regexp"},

		// Save binary formats to a file
		"callgrind": {c, report.Callgrind, awayFromTTY("callgraph.out"), false, "Outputs a graph in callgrind format"},
		"proto":     {c, report.Proto, awayFromTTY("pb.gz"), false, "Outputs the profile in compressed protobuf format"},

		// Generate report in DOT format and postprocess with dot
		"gif": {c, report.Dot, invokeDot("gif"), false, "Outputs a graph image in GIF format"},
		"pdf": {c, report.Dot, invokeDot("pdf"), false, "Outputs a graph in PDF format"},
		"png": {c, report.Dot, invokeDot("png"), false, "Outputs a graph image in PNG format"},
		"ps":  {c, report.Dot, invokeDot("ps"), false, "Outputs a graph in PS format"},

		// Save SVG output into a file after including svgpan library
		"svg": {c, report.Dot, saveSVGToFile(), false, "Outputs a graph in SVG format"},

		// Visualize postprocessed dot output
		"eog":    {c, report.Dot, invokeVisualizer(interactive, invokeDot("svg"), "svg", []string{"eog"}), false, "Visualize graph through eog"},
		"evince": {c, report.Dot, invokeVisualizer(interactive, invokeDot("pdf"), "pdf", []string{"evince"}), false, "Visualize graph through evince"},
		"gv":     {c, report.Dot, invokeVisualizer(interactive, invokeDot("ps"), "ps", []string{"gv --noantialias"}), false, "Visualize graph through gv"},
		"web":    {c, report.Dot, invokeVisualizer(interactive, saveSVGToFile(), "svg", browsers()), false, "Visualize graph through web browser"},

		// Visualize HTML directly generated by report.
		"weblist": {c, report.WebList, invokeVisualizer(interactive, awayFromTTY("html"), "html", browsers()), true, "Output annotated source in HTML for functions matching regexp or address"},
	}
}

// browsers returns a list of commands to attempt for web visualization
// on the current platform
func browsers() []string {
	var cmds []string
	if exe := os.Getenv("BROWSER"); exe != "" {
		cmds = append(cmds, exe)
	}
	switch runtime.GOOS {
	case "darwin":
		cmds = append(cmds, "/usr/bin/open")
	case "windows":
		cmds = append(cmds, "cmd /c start")
	default:
		cmds = append(cmds, "xdg-open")
	}
	cmds = append(cmds, "chrome", "google-chrome", "firefox")
	return cmds
}

// NewCompleter creates an autocompletion function for a set of commands.
func NewCompleter(cs Commands) Completer {
	return func(line string) string {
		switch tokens := strings.Fields(line); len(tokens) {
		case 0:
			// Nothing to complete
		case 1:
			// Single token -- complete command name
			found := ""
			for c := range cs {
				if strings.HasPrefix(c, tokens[0]) {
					if found != "" {
						return line
					}
					found = c
				}
			}
			if found != "" {
				return found
			}
		default:
			// Multiple tokens -- complete using command completer
			if c, ok := cs[tokens[0]]; ok {
				if c.Complete != nil {
					lastTokenIdx := len(tokens) - 1
					lastToken := tokens[lastTokenIdx]
					if strings.HasPrefix(lastToken, "-") {
						lastToken = "-" + c.Complete(lastToken[1:])
					} else {
						lastToken = c.Complete(lastToken)
					}
					return strings.Join(append(tokens[:lastTokenIdx], lastToken), " ")
				}
			}
		}
		return line
	}
}

// awayFromTTY saves the output in a file if it would otherwise go to
// the terminal screen. This is used to avoid dumping binary data on
// the screen.
func awayFromTTY(format string) PostProcessor {
	return func(input *bytes.Buffer, output io.Writer, ui plugin.UI) error {
		if output == os.Stdout && ui.IsTerminal() {
			tempFile, err := tempfile.New("", "profile", "."+format)
			if err != nil {
				return err
			}
			ui.PrintErr("Generating report in ", tempFile.Name())
			_, err = fmt.Fprint(tempFile, input)
			return err
		}
		_, err := fmt.Fprint(output, input)
		return err
	}
}

func invokeDot(format string) PostProcessor {
	divert := awayFromTTY(format)
	return func(input *bytes.Buffer, output io.Writer, ui plugin.UI) error {
		if _, err := exec.LookPath("dot"); err != nil {
			ui.PrintErr("Cannot find dot, have you installed Graphviz?")
			return err
		}
		cmd := exec.Command("dot", "-T"+format)
		var buf bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = input, &buf, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return divert(&buf, output, ui)
	}
}

func saveSVGToFile() PostProcessor {
	generateSVG := invokeDot("svg")
	divert := awayFromTTY("svg")
	return func(input *bytes.Buffer, output io.Writer, ui plugin.UI) error {
		baseSVG := &bytes.Buffer{}
		generateSVG(input, baseSVG, ui)
		massaged := &bytes.Buffer{}
		fmt.Fprint(massaged, svg.Massage(*baseSVG))
		return divert(massaged, output, ui)
	}
}

var vizTmpDir string

func makeVizTmpDir() error {
	if vizTmpDir != "" {
		return nil
	}
	name, err := ioutil.TempDir("", "pprof-")
	if err != nil {
		return err
	}
	tempfile.DeferDelete(name)
	vizTmpDir = name
	return nil
}

func invokeVisualizer(interactive **bool, format PostProcessor, suffix string, visualizers []string) PostProcessor {
	return func(input *bytes.Buffer, output io.Writer, ui plugin.UI) error {
		if err := makeVizTmpDir(); err != nil {
			return err
		}
		tempFile, err := tempfile.New(vizTmpDir, "pprof", "."+suffix)
		if err != nil {
			return err
		}
		tempfile.DeferDelete(tempFile.Name())
		if err = format(input, tempFile, ui); err != nil {
			return err
		}
		tempFile.Close() // on windows, if the file is Open, start cannot access it.
		// Try visualizers until one is successful
		for _, v := range visualizers {
			// Separate command and arguments for exec.Command.
			args := strings.Split(v, " ")
			if len(args) == 0 {
				continue
			}
			viewer := exec.Command(args[0], append(args[1:], tempFile.Name())...)
			viewer.Stderr = os.Stderr
			if err = viewer.Start(); err == nil {
				// The viewer might just send a message to another program
				// to open the file. Give that program a little time to open the
				// file before we remove it.
				time.Sleep(1 * time.Second)

				if !**interactive {
					// In command-line mode, wait for the viewer to be closed
					// before proceeding
					return viewer.Wait()
				}
				return nil
			}
		}
		return err
	}
}
