// Copyright © 2016 Sidharth Kshatriya
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Masterminds/semver"
	"github.com/cyrus-and/gdb"
	"github.com/fatih/color"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	dontbugCstepLineNumTemp int = 91
	dontbugCstepLineNum     int = 99
	dontbugCpathStartsAt    int = 6
	dontbugMasterBp             = "1"

	statusStarting engineStatus = "starting"
	statusStopping engineStatus = "stopping"
	statusStopped  engineStatus = "stopped"
	statusRunning  engineStatus = "running"
	statusBreak    engineStatus = "break"

	reasonOk         engineReason = "ok"
	reasonError      engineReason = "error"
	reasonAborted    engineReason = "aborted"
	reasonExeception engineReason = "exception"
)

var (
	Verbose              bool
	ShowGdbNotifications bool
)

type engineState struct {
	breakStopNotify chan string
	gdbSession      *gdb.Gdb
	ideConnection   net.Conn
	rrFile          *os.File
	rrCmd           *exec.Cmd
	entryFilePHP    string
	lastSequenceNum int
	status          engineStatus
	reason          engineReason
	featureMap      map[string]engineFeatureValue
	breakpoints     map[string]*engineBreakPoint
	sourceMap       map[string]int
	maxStackDepth   int
	levelAr         []int
}

type engineStatus string
type engineReason string

type dbgpCmd struct {
	Command     string // only the command name eg. stack_get
	FullCommand string // just the options after the command name
	Options     map[string]string
	Sequence    int
}

func sendGdbCommand(gdbSession *gdb.Gdb, command string, arguments ...string) map[string]interface{} {
	if Verbose {
		color.Green("dontbug -> gdb: %v %v", command, strings.Join(arguments, " "))
	}
	result, err := gdbSession.Send(command, arguments...)
	if err != nil {
		log.Fatal(err)
	}

	if Verbose {
		continued := ""
		if len(result) > 300 {
			continued = "..."
		}
		color.Cyan("gdb -> dontbug: %.300v%v", result, continued)
	}
	return result
}

func sendGdbCommandNoisy(gdbSession *gdb.Gdb, command string, arguments ...string) map[string]interface{} {
	originalNoisy := Verbose
	Verbose = true
	result := sendGdbCommand(gdbSession, command, arguments...)
	Verbose = originalNoisy
	return result
}

// a gdb string response looks like '0x7f261d8624e8 "some string here"'
// empty string looks '0x7f44a33a9c1e ""'
func parseGdbStringResponse(gdbResponse string) (string, error) {
	first := strings.Index(gdbResponse, "\"")
	last := strings.LastIndex(gdbResponse, "\"")

	if first == last || first == -1 || last == -1 {
		return "", errors.New("Improper gdb data-evaluate-expression string response to: " + gdbResponse)
	}

	unquote := unquoteGdbStringResult(gdbResponse[first+1 : last])
	return unquote, nil
}

func unquoteGdbStringResult(input string) string {
	l := len(input)
	var buf bytes.Buffer
	skip := false
	for i, c := range input {
		if skip {
			skip = false
			continue
		}

		if c == '\\' && i < l && input[i+1] == '"' {
			buf.WriteRune('"')
			skip = true
		} else {
			buf.WriteRune(c)
		}
	}

	return buf.String()
}

func parseCommand(fullCommand string) dbgpCmd {
	components := strings.Fields(fullCommand)
	flags := make(map[string]string)
	command := components[0]
	for i, v := range components[1:] {
		if i%2 == 1 {
			continue
		}

		// Also remove the leading "-" in the flag via [1:]
		if i+2 < len(components) {
			flags[strings.TrimSpace(v)[1:]] = strings.TrimSpace(components[i+2])
		} else {
			flags[strings.TrimSpace(v)[1:]] = ""
		}
	}

	seq, ok := flags["i"]
	if !ok {
		log.Fatal("Could not find sequence number in command")
	}

	seqInt, err := strconv.Atoi(seq)
	if err != nil {
		log.Fatal(err)
	}

	return dbgpCmd{command, fullCommand, flags, seqInt}
}

func xSlashSgdb(gdbSession *gdb.Gdb, expression string) string {
	resultString := xGdbCmdValue(gdbSession, expression)
	finalString, err := parseGdbStringResponse(resultString)
	if err != nil {
		log.Fatal(finalString)
	}
	return finalString

}

func xSlashDgdb(gdbSession *gdb.Gdb, expression string) int {
	resultString := xGdbCmdValue(gdbSession, expression)
	intResult, err := strconv.Atoi(resultString)
	if err != nil {
		log.Fatal(err)
	}
	return intResult
}

func xGdbCmdValue(gdbSession *gdb.Gdb, expression string) string {
	result := sendGdbCommand(gdbSession, "data-evaluate-expression", expression)
	class, ok := result["class"]

	commandWas := "data-evaluate-expression " + expression
	if !ok {
		sendGdbCommand(gdbSession, "thread-info")
		log.Fatal("Could not execute the gdb/mi command: ", commandWas)
	}

	if class != "done" {
		sendGdbCommand(gdbSession, "thread-info")
		log.Fatal("Could not execute the gdb/mi command: ", commandWas)
	}

	payload := result["payload"].(map[string]interface{})
	resultString := payload["value"].(string)

	return resultString
}

// Returns breakpoint id, true if stopped on a PHP breakpoint
func continueExecution(es *engineState, reverse bool) (string, bool) {
	es.status = statusRunning
	if reverse {
		sendGdbCommand(es.gdbSession, "exec-continue", "--reverse")
	} else {
		sendGdbCommand(es.gdbSession, "exec-continue")
	}

	// Wait for the corresponding breakpoint hit break id
	breakId := <-es.breakStopNotify
	es.status = statusBreak

	// Probably not a good idea to pass out breakId for a breakpoint that is gone
	// But we're not using breakId currently
	if isEnabledPhpTemporaryBreakpoint(es, breakId) {
		delete(es.breakpoints, breakId)
		return breakId, true
	}

	if isEnabledPhpBreakpoint(es, breakId) {
		return breakId, true
	}

	return breakId, false
}

func constructDbgpPacket(payload string) []byte {
	header_xml := "<?xml version=\"1.0\" encoding=\"iso-8859-1\"?>\n"
	var buf bytes.Buffer
	buf.WriteString(strconv.Itoa(len(payload) + len(header_xml)))
	buf.Write([]byte{0})
	buf.WriteString(header_xml)
	buf.WriteString(payload)
	buf.Write([]byte{0})
	return buf.Bytes()
}

func makeNoisy(f func(*engineState, dbgpCmd) string, es *engineState, dCmd dbgpCmd) string {
	originalNoisy := Verbose
	Verbose = true
	result := f(es, dCmd)
	Verbose = originalNoisy
	return result
}

// Output a fatal error if there is anything wrong with path
// Otherwise output the absolute path of the directory/file
func getAbsPathOrFatal(path string) string {
	// Create an absolute path for the path directory/file
	absPath, err := filepath.Abs(path)
	if err != nil {
		log.Fatal(err)
	}

	// Does the directory/file even exist?
	_, err = os.Stat(absPath)
	if err != nil {
		log.Fatal(err)
	}

	return absPath
}

func findExec(file string) (string, error) {
	path, err := exec.LookPath(file)
	name := filepath.Base(file)

	if err != nil {
		return "", errors.New(fmt.Sprintf("Could not find %v. %v", file, err))
	}

	// @TODO remove this in future?
	color.Green("dontbug: Using %v from path %v", name, path)

	return path, nil
}

func checkPhpExecutable(phpExecutable string) string {
	fmt.Println("dontbug: Checking PHP requirements")
	path, firstLine := getPathAndVersionLineOrFatal(phpExecutable)
	versionString := strings.Split(firstLine, " ")[1]

	ver, err := semver.NewVersion(versionString)
	if err != nil {
		log.Fatal(err)
	}

	constraint, err := semver.NewConstraint("~7.0")
	if err != nil {
		log.Fatal(err)
	}

	if !constraint.Check(ver) {
		log.Fatalf("Only PHP 7.x supported. Version %v was given.", versionString)
	}

	matched, err := regexp.MatchString("\\(.*DEBUG.*\\)", firstLine)
	if err != nil {
		log.Fatal(err)
	}

	if !matched {
		log.Fatalf("PHP must be compiled in DEBUG mode. Got: %v", firstLine)
	}

	return path
}

func CheckRRExecutable(rrExecutable string) string {
	fmt.Println("dontbug: Checking rr requirements")
	path, firstLine := getPathAndVersionLineOrFatal(rrExecutable)

	spaceAr := strings.Split(firstLine, " ")
	versionString := spaceAr[len(spaceAr)-1]

	ver, err := semver.NewVersion(versionString)
	if err != nil {
		log.Fatal(err)
	}

	constraint, err := semver.NewConstraint(">= 4.3.0")
	if err != nil {
		log.Fatal(err)
	}

	if !constraint.Check(ver) {
		log.Fatalf("Only rr >= 4.3.0 supported. Version %v was given", versionString)
	}

	return path
}

func CheckGdbExecutable(gdbExecutable string) string {
	fmt.Println("dontbug: Checking gdb requirements")
	path, firstLine := getPathAndVersionLineOrFatal(gdbExecutable)

	spaceAr := strings.Split(firstLine, " ")
	versionString := spaceAr[len(spaceAr)-1]

	ver, err := semver.NewVersion(versionString)
	if err != nil {
		log.Fatal(err)
	}

	constraint, err := semver.NewConstraint(">= 7.11.1")
	if err != nil {
		log.Fatal(err)
	}

	if !constraint.Check(ver) {
		log.Fatalf("Only gdb >= 7.11.1 supported. Version %v was given", versionString)
	}

	return path
}

func getPathAndVersionLineOrFatal(file string) (string, string) {
	path, err := findExec(file)
	if err != nil {
		log.Fatal(err)
	}

	output, err := exec.Command(path, "--version").Output()
	if err != nil {
		log.Fatal(err)
	}

	outString := string(output)
	firstLine := strings.Split(outString, "\n")[0]

	return path, firstLine
}
