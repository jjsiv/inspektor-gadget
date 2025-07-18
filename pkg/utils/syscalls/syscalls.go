// Copyright 2023 The Inspektor Gadget authors
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

package syscalls

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func GetSyscallNumberByName(name string) (int, bool) {
	number, ok := syscallsNameToNumber[name]

	return number, ok
}

func GetSyscallNameByNumber(number int) (string, bool) {
	name, ok := syscallsNumberToName[number]

	return name, ok
}

const syscallsPath = `/sys/kernel/debug/tracing/events/syscalls/`

type param struct {
	position  int
	name      string
	isPointer bool
}

type SyscallDeclaration struct {
	name   string
	params []param
}

func SyscallGetName(nr uint16) string {
	name, ok := GetSyscallNameByNumber(int(nr))
	// Just do like strace (https://man7.org/linux/man-pages/man1/strace.1.html):
	// Syscalls unknown to strace are printed raw
	if !ok {
		return fmt.Sprintf("syscall_%x", nr)
	}

	return name
}

var re = regexp.MustCompile(`\s+field:(?P<type>.*?) (?P<name>[a-z_0-9]+);.*`)

func parseLine(l string, idx int) (*param, error) {
	n1 := re.SubexpNames()

	r := re.FindAllStringSubmatch(l, -1)
	if len(r) == 0 {
		return nil, nil
	}
	res := r[0]

	mp := map[string]string{}
	for i, n := range res {
		mp[n1[i]] = n
	}

	if _, ok := mp["type"]; !ok {
		return nil, nil
	}
	if _, ok := mp["name"]; !ok {
		return nil, nil
	}

	// ignore
	if mp["name"] == "__syscall_nr" {
		return nil, nil
	}

	var cParam param
	cParam.name = mp["name"]

	// The position is calculated based on the event format. The actual parameters
	// start from 8th index, hence we subtract that from idx to get position
	// of the parameter to the syscall
	cParam.position = idx - 8

	cParam.isPointer = strings.Contains(mp["type"], "*")

	return &cParam, nil
}

// Map sys_enter_NAME to syscall name as in /usr/include/asm/unistd_64.h
// TODO Check if this is also true for arm64.
func relateSyscallName(name string) string {
	switch name {
	case "newfstat":
		return "fstat"
	case "newlstat":
		return "lstat"
	case "newstat":
		return "stat"
	case "newuname":
		return "uname"
	case "sendfile64":
		return "sendfile"
	case "sysctl":
		return "_sysctl"
	case "umount":
		return "umount2"
	default:
		return name
	}
}

func parseSyscall(name, format string) (*SyscallDeclaration, error) {
	syscallParts := strings.Split(format, "\n")
	var skipped bool

	var cParams []param
	for idx, line := range syscallParts {
		if !skipped {
			if len(line) != 0 {
				continue
			} else {
				skipped = true
			}
		}
		cp, err := parseLine(line, idx)
		if err != nil {
			return nil, err
		}
		if cp != nil {
			cParams = append(cParams, *cp)
		}
	}

	return &SyscallDeclaration{
		name:   name,
		params: cParams,
	}, nil
}

func GatherSyscallsDeclarations() (map[string]SyscallDeclaration, error) {
	cSyscalls := make(map[string]SyscallDeclaration)
	err := filepath.Walk(syscallsPath, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if path == "syscalls" {
			return nil
		}

		if !f.IsDir() {
			return nil
		}

		eventName := f.Name()
		if strings.HasPrefix(eventName, "sys_exit") {
			return nil
		}

		syscallName := strings.TrimPrefix(eventName, "sys_enter_")
		syscallName = relateSyscallName(syscallName)

		formatFilePath := filepath.Join(syscallsPath, eventName, "format")
		formatFile, err := os.Open(formatFilePath)
		if err != nil {
			return nil
		}
		defer formatFile.Close()

		formatBytes, err := io.ReadAll(formatFile)
		if err != nil {
			return err
		}

		cSyscall, err := parseSyscall(syscallName, string(formatBytes))
		if err != nil {
			return err
		}

		cSyscalls[cSyscall.name] = *cSyscall

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %q: %w", syscallsPath, err)
	}
	return cSyscalls, nil
}

func GetSyscallDeclaration(syscallsDeclarations map[string]SyscallDeclaration, syscallName string) (SyscallDeclaration, error) {
	declaration, ok := syscallsDeclarations[syscallName]
	if !ok {
		return SyscallDeclaration{}, fmt.Errorf("no syscall correspond to %q", syscallName)
	}

	return declaration, nil
}

func (s SyscallDeclaration) GetParameterCount() uint8 {
	return uint8(len(s.params))
}

func (s SyscallDeclaration) ParamIsPointer(paramNumber uint8) (bool, error) {
	if int(paramNumber) >= len(s.params) {
		return false, fmt.Errorf("param number %d out of bounds for syscall %q", paramNumber, s.name)
	}
	return s.params[paramNumber].isPointer, nil
}

func (s SyscallDeclaration) GetParameterName(paramNumber uint8) (string, error) {
	if int(paramNumber) >= len(s.params) {
		return "", fmt.Errorf("param number %d out of bounds for syscall %q", paramNumber, s.name)
	}
	return s.params[paramNumber].name, nil
}
