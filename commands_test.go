package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

// dispatchedCommands returns the command names handle switches on, with the
// first name of each case.
func dispatchedCommands(t *testing.T) (names, first []string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Name.Name != "handle" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for i, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				name, _ := strconv.Unquote(literal.Value)
				names = append(names, name)
				if i == 0 {
					first = append(first, name)
				}
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatal("found no commands in handle")
	}
	return names, first
}

func TestBotCommandsMatchTheCommandsHandled(t *testing.T) {
	names, first := dispatchedCommands(t)
	valid := regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	listed := make(map[string]bool)
	for _, command := range botCommands {
		if !valid.MatchString(command.name) || len(command.description) < 3 || len(command.description) > 256 {
			t.Errorf("Telegram would refuse %+v", command)
		}
		for _, name := range []string{command.name, command.alias} {
			if name == "" {
				continue
			}
			if listed[name] {
				t.Errorf("%s is listed twice", name)
			}
			listed[name] = true
			if !slices.Contains(names, name) {
				t.Errorf("%s is listed but not handled", name)
			}
		}
		if !slices.Contains(first, command.name) {
			t.Errorf("%s is not the first name of its case in handle", command.name)
		}
	}
	// hashing is the old name of checking.
	for _, name := range names {
		if !listed[name] && name != "hashing" && name != "ha" {
			t.Errorf("%s is handled but not listed", name)
		}
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	for _, command := range botCommands {
		if !strings.Contains(helpText, "/"+command.name+" ") {
			t.Errorf("help lacks %s", command.name)
		}
	}
}

func TestREADMEHasTheBotFatherCommandList(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	block := "```text\n" + botFatherCommands() + "```"
	if !strings.Contains(strings.ReplaceAll(string(readme), "\r\n", "\n"), block) {
		t.Fatalf("README.md lacks the BotFather list; paste this in:\n%s", block)
	}
}

func TestRegisterCommandsFillsOnlyAnEmptyMenu(t *testing.T) {
	telegramFake := &fakeTelegram{}
	var logs strings.Builder
	app := &application{bot: newTestBot(t, telegramFake, "123:SECRET"), logger: log.New(&logs, "", 0), token: "123:SECRET"}
	app.registerCommands(context.Background())
	var menu []models.BotCommand
	if err := json.Unmarshal([]byte(telegramFake.commands), &menu); err != nil || len(menu) != len(botCommands) {
		t.Fatalf("registered %s, %v; log: %s", telegramFake.commands, err, logs.String())
	}
	if menu[0].Command != botCommands[0].name || menu[0].Description != botCommands[0].description {
		t.Fatalf("first command = %+v", menu[0])
	}

	telegramFake.commands, telegramFake.methods = `[{"command":"mine","description":"set in BotFather"}]`, nil
	app.registerCommands(context.Background())
	if !strings.Contains(telegramFake.commands, "mine") || slices.Contains(telegramFake.methods, "setMyCommands") {
		t.Fatalf("replaced a menu set in BotFather: %s", telegramFake.commands)
	}
}
