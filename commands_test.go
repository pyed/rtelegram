package main

import (
	"cmp"
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

// The bot fills an empty command menu and keeps its own up to date, but not
// a menu set in BotFather.
func TestRegisterCommandsKeepsItsOwnMenuCurrent(t *testing.T) {
	telegramFake := &fakeTelegram{}
	var logs strings.Builder
	app := &application{bot: newTestBot(t, telegramFake, "123:SECRET"), logger: log.New(&logs, "", 0), token: "123:SECRET", state: &state{}}
	register := func(current string) (menu []models.BotCommand, set bool) {
		t.Helper()
		telegramFake.commands, telegramFake.methods = current, nil
		app.registerCommands(context.Background())
		if err := json.Unmarshal([]byte(cmp.Or(telegramFake.commands, "[]")), &menu); err != nil {
			t.Fatalf("menu %s: %v; log: %s", telegramFake.commands, err, logs.String())
		}
		return menu, slices.Contains(telegramFake.methods, "setMyCommands")
	}
	menuJSON := func(menu []models.BotCommand) string {
		encoded, _ := json.Marshal(menu)
		return string(encoded)
	}

	menu, set := register("")
	if !set || len(menu) != len(botCommands) || menu[0].Command != botCommands[0].name || menu[0].Description != botCommands[0].description {
		t.Fatalf("filled an empty menu with %+v", menu)
	}
	app.state.read(func(data *stateData) {
		if !slices.Equal(data.Menu, commandNames(menu)) {
			t.Fatalf("remembered registering %v", data.Menu)
		}
	})
	current := menuJSON(menu)
	if _, set := register(current); set {
		t.Fatal("replaced a menu that was up to date")
	}

	// An earlier version's menu, with other descriptions, is brought up to
	// date, even by a bot that did not yet remember registering it.
	if err := app.state.update(func(data *stateData) { data.Menu = nil }); err != nil {
		t.Fatal(err)
	}
	outdated := slices.Clone(menu)
	outdated[0].Description = "an older description"
	if menu, set := register(menuJSON(outdated)); !set || menuJSON(menu) != current {
		t.Fatalf("an outdated menu became %+v", menu)
	}
	// So is the menu the bot last registered, though this version's commands
	// differ from it.
	if err := app.state.update(func(data *stateData) { data.Menu = []string{"list", "help"} }); err != nil {
		t.Fatal(err)
	}
	earlier := `[{"command":"list","description":"List torrents"},{"command":"help","description":"List the commands"}]`
	if menu, set := register(earlier); !set || menuJSON(menu) != current {
		t.Fatalf("the menu registered earlier became %+v", menu)
	}

	for _, own := range []string{`[{"command":"mine","description":"set in BotFather"}]`, earlier} {
		if menu, set := register(own); set || menuJSON(menu) != own {
			t.Fatalf("replaced a menu set in BotFather, %s, with %+v", own, menu)
		}
	}
}
