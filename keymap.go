package main

// keymap turns Linux input event codes (the tca8418 matrix keyboard reports
// ordinary KEY_* codes) into runes.

import "strings"

const (
	keyEsc        = 1
	keyBackspace  = 14
	keyTab        = 15
	keyEnter      = 28
	keyLeftCtrl   = 29
	keyLeftShift  = 42
	keyRightShift = 54
	keySpace      = 57
)

var plain = map[uint16]rune{
	2: '1', 3: '2', 4: '3', 5: '4', 6: '5', 7: '6', 8: '7', 9: '8', 10: '9', 11: '0',
	12: '-', 13: '=',
	16: 'q', 17: 'w', 18: 'e', 19: 'r', 20: 't', 21: 'y', 22: 'u', 23: 'i', 24: 'o', 25: 'p',
	26: '[', 27: ']',
	30: 'a', 31: 's', 32: 'd', 33: 'f', 34: 'g', 35: 'h', 36: 'j', 37: 'k', 38: 'l',
	39: ';', 40: '\'', 41: '`', 43: '\\',
	44: 'z', 45: 'x', 46: 'c', 47: 'v', 48: 'b', 49: 'n', 50: 'm',
	51: ',', 52: '.', 53: '/',
	57:     ' ',
	keyTab: '\t',
}

var shifted = map[uint16]rune{
	2: '!', 3: '@', 4: '#', 5: '$', 6: '%', 7: '^', 8: '&', 9: '*', 10: '(', 11: ')',
	12: '_', 13: '+', 26: '{', 27: '}', 39: ':', 40: '"', 41: '~', 43: '|',
	51: '<', 52: '>', 53: '?',
}

// keyRune returns the rune for a press of code, and whether it is printable.
func keyRune(code uint16, shift bool) (rune, bool) {
	if shift {
		if r, ok := shifted[code]; ok {
			return r, true
		}
		if r, ok := plain[code]; ok {
			return []rune(strings.ToUpper(string(r)))[0], true
		}
		return 0, false
	}
	if r, ok := plain[code]; ok {
		return r, true
	}
	return 0, false
}
