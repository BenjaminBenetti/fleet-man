package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	glyphHeight  = 5
	bannerHeight = 6
)

// Character map for simple block letters — each glyph has a 5-line body.
var charMap = map[rune][glyphHeight]string{
	'a': {"       ", "  __ _ ", " / _` |", "| (_| |", " \\__,_|"},
	'b': {" _     ", "| |__  ", "| '_ \\ ", "| |_) |", "|_.__/ "},
	'c': {"      ", "  ___ ", " / __|", "| (__ ", " \\___|"},
	'd': {"     _ ", "  __| |", " / _` |", "| (_| |", " \\__,_|"},
	'e': {"      ", "  ___ ", " / _ \\", "|  __/", " \\___|"},
	'f': {" __ ", "/ _|", "| | ", "| | ", "|_| "},
	'g': {"       ", "  __ _ ", " / _` |", "| (_| |", " \\__, |"},
	'h': {" _     ", "| |__  ", "| '_ \\ ", "| | | |", "|_| |_|"},
	'i': {" _ ", "(_)", "| |", "| |", "|_|"},
	'j': {"   _ ", "  (_)", "  | |", "  | |", " _/ |"},
	'k': {" _    ", "| | __", "| |/ /", "|   < ", "|_|\\_\\"},
	'l': {" _ ", "| |", "| |", "| |", "|_|"},
	'm': {"            ", " _ __ ___   ", "| '_ ` _ \\  ", "| | | | | | ", "|_| |_| |_| "},
	'n': {"        ", " _ __   ", "| '_ \\  ", "| | | | ", "|_| |_| "},
	'o': {"       ", "  ___  ", " / _ \\ ", "| (_) |", " \\___/ "},
	'p': {"       ", " _ __  ", "| '_ \\ ", "| |_) |", "| .__/ "},
	'q': {"       ", "  __ _ ", " / _` |", "| (_| |", " \\__, |"},
	'r': {"      ", " _ __ ", "| '__|", "| |   ", "|_|   "},
	's': {"     ", " ___ ", "/ __|", "\\__ \\", "|___/"},
	't': {" _   ", "| |_ ", "| __|", "| |_ ", " \\__|"},
	'u': {"       ", " _   _ ", "| | | |", "| |_| |", " \\__,_|"},
	'v': {"       ", "__   __", "\\ \\ / /", " \\ V / ", "  \\_/  "},
	'w': {"              ", "__      __    ", "\\ \\ /\\ / /    ", " \\ V  V /     ", "  \\_/\\_/      "},
	'x': {"      ", "__  __", "\\ \\/ /", " >  < ", "/_/\\_\\"},
	'y': {"       ", " _   _ ", "| | | |", "| |_| |", " \\__, |"},
	'z': {"     ", " ____", "|_  /", " / / ", "/___|"},
	'0': {"  ___  ", " / _ \\ ", "| | | |", "| |_| |", " \\___/ "},
	'1': {" _ ", "/ |", "| |", "| |", "|_|"},
	'2': {" ____  ", "|___ \\ ", "  __) |", " / __/ ", "|_____|"},
	'3': {" _____ ", "|___ / ", "  |_ \\ ", " ___) |", "|____/ "},
	'4': {" _  _   ", "| || |  ", "| || |_ ", "|__   _|", "   |_|  "},
	'5': {" ____  ", "| ___| ", "|___ \\ ", " ___) |", "|____/ "},
	'6': {"  __   ", " / /_  ", "| '_ \\ ", "| (_) |", " \\___/ "},
	'7': {" _____ ", "|___  |", "   / / ", "  / /  ", " /_/   "},
	'8': {"  ___  ", " ( _ ) ", " / _ \\ ", "| (_) |", " \\___/ "},
	'9': {"  ___  ", " / _ \\ ", "| (_) |", " \\__, |", "   /_/ "},
	'-': {"     ", "     ", " ___ ", "|___|", "     "},
	'/': {"    __", "   / /", "  / / ", " / /  ", "/_/   "},
	'.': {"   ", "   ", "   ", " _ ", "(_)"},
	'_': {"      ", "      ", "      ", " ____ ", "|____|"},
	' ': {"  ", "  ", "  ", "  ", "  "},
}

var descenderMap = map[rune]string{
	'g': " |___/ ",
	'q': "    |_|",
	'y': " |___/ ",
}

// nameToBanner converts a string into 6-line ASCII art with a descender row.
func nameToBanner(name string) string {
	name = strings.ToLower(name)
	lines := [bannerHeight]strings.Builder{}

	for _, ch := range name {
		glyph, ok := charMap[ch]
		if !ok {
			glyph = charMap[' ']
		}
		for i := 0; i < glyphHeight; i++ {
			lines[i].WriteString(glyph[i])
		}
		lines[glyphHeight].WriteString(descenderFor(ch, len(glyph[0])))
	}

	var result strings.Builder
	for i := 0; i < bannerHeight; i++ {
		if i > 0 {
			result.WriteString("\n")
		}
		result.WriteString(lines[i].String())
	}
	return result.String()
}

func descenderFor(ch rune, width int) string {
	descender, ok := descenderMap[ch]
	if !ok {
		return strings.Repeat(" ", width)
	}
	if len(descender) < width {
		return descender + strings.Repeat(" ", width-len(descender))
	}
	return descender
}

// execWithBannerCmd returns an *exec.Cmd that clears the screen, prints a banner, then execs the given command.
func execWithBannerCmd(banner string, cmd *exec.Cmd) *exec.Cmd {
	// Write the banner to a temp file so the shell can cat it
	f, err := os.CreateTemp("", "fleet-banner-*.txt")
	if err != nil {
		return cmd
	}
	fmt.Fprintln(f, banner)
	fmt.Fprintln(f)
	f.Close()

	// Build a shell command: clear, print banner, exec into container
	script := fmt.Sprintf(
		`clear; cat '%s'; rm -f '%s'; exec %s`,
		f.Name(), f.Name(), quoteArgs(cmd.Args),
	)

	return exec.Command("sh", "-c", script)
}
