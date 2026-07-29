package main

import (
	"bytes"
	_ "embed"
	"image/color"
	"log"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
)

//go:embed assets/PressStart2P-Regular.ttf
var pressStart2P []byte

var (
	fontSource *text.GoTextFaceSource
	fontSmall  *text.GoTextFace
	fontBig    *text.GoTextFace
)

func loadFonts() {
	source, err := text.NewGoTextFaceSource(bytes.NewReader(pressStart2P))
	if err != nil {
		log.Fatalf("load font: %v", err)
	}
	fontSource = source
	fontSmall = &text.GoTextFace{Source: fontSource, Size: 8}
	fontBig = &text.GoTextFace{Source: fontSource, Size: 16}
}

// Palette — Battle City-inspired, NES-ish tones. All art below is original
// pixel work in that style (the actual NES sprites are copyrighted).
var palette = map[rune]color.RGBA{
	'Y': {228, 176, 52, 255},  // tank body
	'y': {252, 228, 116, 255}, // tank highlight
	'D': {136, 104, 24, 255},  // treads
	'd': {84, 64, 16, 255},    // tread notches
	'B': {200, 76, 12, 255},   // brick
	'b': {148, 52, 4, 255},    // brick shade
	'M': {60, 56, 52, 255},    // mortar
	'S': {188, 188, 188, 255}, // steel
	's': {116, 116, 116, 255}, // steel shade
	'W': {252, 252, 252, 255}, // white / steel shine / bullet
	'O': {252, 152, 56, 255},  // explosion orange
	'R': {216, 40, 0, 255},    // explosion red
}

func spriteFromArt(art []string) *ebiten.Image {
	h := len(art)
	w := len(art[0])
	img := ebiten.NewImage(w, h)
	for y, row := range art {
		for x, ch := range row {
			if ch == '.' {
				continue
			}
			img.Set(x, y, palette[ch])
		}
	}
	return img
}

// tankArt faces up; the other three directions are GeoM rotations.
var tankArt = []string{
	".......yy.......",
	".......yY.......",
	".......yY.......",
	"Dd.....yY.....Dd",
	"dD.yyyyyYYYYy.dD",
	"Dd.yYYYYYYYYY.Dd",
	"dD.yYYYYYYYYY.dD",
	"DdyYYYDDDDYYYYDd",
	"dDyYYDYYYYDYYYdD",
	"DdyYYDYYYYDYYYDd",
	"dDyYYDYYYYDYYYdD",
	"DdyYYYDDDDYYYYDd",
	"dD.yYYYYYYYYY.dD",
	"Dd.yYYYYYYYYY.Dd",
	"dD..yyyyyyyy..dD",
	"Dd............Dd",
}

var brickArt = []string{
	"BBBBBBBMBBBBBBBM",
	"BbBBBbBMBBbBBBbM",
	"BBBBBBBMBBBBBBBM",
	"MMMMMMMMMMMMMMMM",
	"BBBMBBBBBBBMBBBB",
	"BbBMBBbBBBbMBBBb",
	"BBBMBBBBBBBMBBBB",
	"MMMMMMMMMMMMMMMM",
	"BBBBBBBMBBBBBBBM",
	"BbBBBbBMBBbBBBbM",
	"BBBBBBBMBBBBBBBM",
	"MMMMMMMMMMMMMMMM",
	"BBBMBBBBBBBMBBBB",
	"BbBMBBbBBBbMBBBb",
	"BBBMBBBBBBBMBBBB",
	"MMMMMMMMMMMMMMMM",
}

var steelArt = []string{
	"SSSSSSSSSSSSSSSs",
	"SWWWWWWWWWWWWSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"SWSSSSSSSSSSSSss",
	"Ssssssssssssssss",
	"ssssssssssssssss",
}

var bulletArt = []string{
	".WW.",
	"WWWW",
	"WWWW",
	".ss.",
}

var explosionArt1 = []string{
	"................",
	".....O..........",
	"..W.....O...W...",
	"....O.OOO.......",
	"......OROO..O...",
	"..O..OORRO......",
	"....OORRRROO.W..",
	"...OORRWRRROO...",
	"..W.ORRWWRRO....",
	"....OORRRROO....",
	"......ORRO...O..",
	"...O..OOO.......",
	".....O....O.....",
	"..W.....W.......",
	".....O..........",
	"................",
}

var explosionArt2 = []string{
	"W......W......W.",
	"..O.........O...",
	"....OOOOOOO.....",
	"..OOORRRRROOO...",
	".OORRRRRRRRROO..",
	".ORRRWWWWWRRRO..",
	"OORRWWWWWWWRROO.",
	"ORRRWWWWWWWRRRO.",
	"OORRWWWWWWWRROO.",
	".ORRRWWWWWRRRO..",
	".OORRRRRRRRROO..",
	"..OOORRRRROOO...",
	"....OOOOOOO.....",
	"..O.........O...",
	"W......W......W.",
	"................",
}

var (
	tankSprite      *ebiten.Image
	brickSprite     *ebiten.Image
	steelSprite     *ebiten.Image
	bulletSprite    *ebiten.Image
	explosionFrames []*ebiten.Image
)

func loadSprites() {
	tankSprite = spriteFromArt(tankArt)
	brickSprite = spriteFromArt(brickArt)
	steelSprite = spriteFromArt(steelArt)
	bulletSprite = spriteFromArt(bulletArt)
	explosionFrames = []*ebiten.Image{spriteFromArt(explosionArt1), spriteFromArt(explosionArt2)}
}

// tankTints color remote tanks so every player is distinguishable; the local
// player keeps the classic yellow (index 0 tint is identity).
var tankTints = [][3]float32{
	{1, 1, 1},          // yellow (self / default)
	{0.75, 0.95, 1.35}, // silver-blue
	{0.65, 1.25, 0.65}, // green
	{1.35, 0.62, 0.62}, // red
	{1.2, 0.7, 1.3},    // purple
	{0.6, 1.15, 1.15},  // teal
	{1.3, 1.05, 0.55},  // orange
	{0.9, 0.9, 0.9},    // gray
}

func tintFor(playerID string) [3]float32 {
	sum := 0
	for _, b := range []byte(playerID) {
		sum += int(b)
	}
	return tankTints[1+sum%(len(tankTints)-1)]
}
