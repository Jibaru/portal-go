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

// ── Tiles (GBA-overworld-style, generated) ────────────────

var (
	grassBase   = color.RGBA{136, 192, 112, 255}
	grassDark   = color.RGBA{104, 168, 88, 255}
	pathBase    = color.RGBA{216, 200, 160, 255}
	pathDark    = color.RGBA{192, 168, 120, 255}
	waterBase   = color.RGBA{80, 128, 224, 255}
	waterLight  = color.RGBA{144, 184, 248, 255}
	treeDark    = color.RGBA{40, 112, 56, 255}
	treeLight   = color.RGBA{80, 152, 88, 255}
	trunkBrown  = color.RGBA{120, 88, 48, 255}
	flowerRed   = color.RGBA{216, 64, 56, 255}
	flowerGold  = color.RGBA{248, 216, 96, 255}
	tuftGreen   = color.RGBA{72, 136, 64, 255}
	uiPanel     = color.RGBA{16, 24, 32, 235}
	uiHighlight = color.RGBA{248, 216, 96, 255}
	uiDim       = color.RGBA{150, 150, 150, 255}
)

func fillTile(img *ebiten.Image, base color.RGBA) {
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			img.Set(x, y, base)
		}
	}
}

func genGrass() *ebiten.Image {
	img := ebiten.NewImage(tileSize, tileSize)
	fillTile(img, grassBase)
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			if (x*3+y*5)%13 == 0 {
				img.Set(x, y, grassDark)
			}
		}
	}
	return img
}

func genPath() *ebiten.Image {
	img := ebiten.NewImage(tileSize, tileSize)
	fillTile(img, pathBase)
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			if (x*7+y*3)%17 == 0 {
				img.Set(x, y, pathDark)
			}
		}
	}
	return img
}

func genTallGrass() *ebiten.Image {
	img := genGrass()
	for _, row := range []int{3, 4, 9, 10, 14, 15} {
		for x := 0; x < tileSize; x++ {
			if (x+row)%4 < 2 {
				img.Set(x, row, tuftGreen)
			}
		}
	}
	return img
}

func genFlower() *ebiten.Image {
	img := genGrass()
	// A little 4-petal flower.
	for _, p := range [][2]int{{7, 5}, {5, 7}, {9, 7}, {7, 9}} {
		img.Set(p[0], p[1], flowerRed)
		img.Set(p[0]+1, p[1], flowerRed)
		img.Set(p[0], p[1]+1, flowerRed)
		img.Set(p[0]+1, p[1]+1, flowerRed)
	}
	img.Set(7, 7, flowerGold)
	img.Set(8, 7, flowerGold)
	img.Set(7, 8, flowerGold)
	img.Set(8, 8, flowerGold)
	return img
}

func genWater() *ebiten.Image {
	img := ebiten.NewImage(tileSize, tileSize)
	fillTile(img, waterBase)
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			if y%8 == 2 && (x+y)%8 < 4 {
				img.Set(x, y, waterLight)
			}
		}
	}
	return img
}

func genTree() *ebiten.Image {
	img := genGrass()
	// Trunk.
	for y := 12; y < 16; y++ {
		for x := 6; x < 10; x++ {
			img.Set(x, y, trunkBrown)
		}
	}
	// Canopy: a rough circle with an upper-left highlight.
	cx, cy, r := 8, 7, 7
	for y := 0; y < tileSize; y++ {
		for x := 0; x < tileSize; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				if dx < 0 && dy < 0 && dx*dx+dy*dy > (r-3)*(r-3) {
					img.Set(x, y, treeLight)
				} else {
					img.Set(x, y, treeDark)
				}
			}
		}
	}
	return img
}

var tileSprites map[tileKind]*ebiten.Image

// ── Characters (original GBA-style pixel art) ─────────────
//
// 16x20 frames; palette letters: O outline, H hair, h hair shade, S skin,
// E eye, C shirt, c shirt shade, P pants, B boots. Right-facing frames are
// left-facing frames mirrored at draw time.

var charDownIdle = []string{
	".....OOOOOO.....",
	"....OHHHHHHO....",
	"...OHHHHHHHHO...",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OHhSSSSSShHO..",
	"..OhSSSSSSSShO..",
	"..OSESSSSSSESO..",
	"..OSSSSSSSSSSO..",
	"...OSSSSSSSSO...",
	"....OSSSSSSO....",
	"...OCCCCCCCCO...",
	"..OCCCCCCCCCCO..",
	"..OSCCCCCCCCSO..",
	"..OScCCCCCCcSO..",
	"...OCCCCCCCCO...",
	"...OPPPPPPPPO...",
	"...OPPPOOPPPO...",
	"...OBBO..OBBO...",
	"....OO....OO....",
}

var charDownStep = []string{
	".....OOOOOO.....",
	"....OHHHHHHO....",
	"...OHHHHHHHHO...",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OHhSSSSSShHO..",
	"..OhSSSSSSSShO..",
	"..OSESSSSSSESO..",
	"..OSSSSSSSSSSO..",
	"...OSSSSSSSSO...",
	"....OSSSSSSO....",
	"...OCCCCCCCCO...",
	"..OCCCCCCCCCCO..",
	"..OSCCCCCCCCSO..",
	"..OScCCCCCCcSO..",
	"...OCCCCCCCCO...",
	"...OPPPPPPPPO...",
	"...OPPOOPPPPO...",
	"..OBBO..OBBBO...",
	"...OO.....OO....",
}

var charUpIdle = []string{
	".....OOOOOO.....",
	"....OHHHHHHO....",
	"...OHHHHHHHHO...",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OhHHHHHHHHhO..",
	"..OhHHHHHHHHhO..",
	"..OSHHHHHHHHSO..",
	"...OSHHHHHHSO...",
	"....OSSSSSSO....",
	"...OCCCCCCCCO...",
	"..OCCCCCCCCCCO..",
	"..OSCCCCCCCCSO..",
	"..OScCCCCCCcSO..",
	"...OCCCCCCCCO...",
	"...OPPPPPPPPO...",
	"...OPPPOOPPPO...",
	"...OBBO..OBBO...",
	"....OO....OO....",
}

var charUpStep = []string{
	".....OOOOOO.....",
	"....OHHHHHHO....",
	"...OHHHHHHHHO...",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OhHHHHHHHHhO..",
	"..OhHHHHHHHHhO..",
	"..OSHHHHHHHHSO..",
	"...OSHHHHHHSO...",
	"....OSSSSSSO....",
	"...OCCCCCCCCO...",
	"..OCCCCCCCCCCO..",
	"..OSCCCCCCCCSO..",
	"..OScCCCCCCcSO..",
	"...OCCCCCCCCO...",
	"...OPPPPPPPPO...",
	"...OPPPPOOPPO...",
	"...OBBBO..OBBO..",
	"....OO.....OO...",
}

var charLeftIdle = []string{
	".....OOOOOO.....",
	"....OHHHHHHO....",
	"...OHHHHHHHHO...",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OShHHHHHHHHO..",
	"..OSSHHHHHHHHO..",
	"..OESSHHHHHHHO..",
	"..OSSSHHHHHHHO..",
	"...OSSHHHHHHO...",
	"....OSSSSSSO....",
	"...OCCCCCCCCO...",
	"...OCCCCCCCCO...",
	"..OSCCCCCCCCO...",
	"...OCCCCCCCCO...",
	"...OPPPPPPPPO...",
	"...OPPPPPPPPO...",
	"...OPPPOPPPO....",
	"...OBBO.OBBO....",
	"....OO...OO.....",
}

var charLeftStep = []string{
	".....OOOOOO.....",
	"....OHHHHHHO....",
	"...OHHHHHHHHO...",
	"..OHHHHHHHHHHO..",
	"..OHHHHHHHHHHO..",
	"..OShHHHHHHHHO..",
	"..OSSHHHHHHHHO..",
	"..OESSHHHHHHHO..",
	"..OSSSHHHHHHHO..",
	"...OSSHHHHHHO...",
	"....OSSSSSSO....",
	"...OCCCCCCCCO...",
	"...OCCCCCCCCO...",
	"..OSCCCCCCCCO...",
	"...OCCCCCCCCO...",
	"...OPPPPPPPPO...",
	"..OPPPPPPPPPO...",
	"..OPPO..OPPPO...",
	"..OBBO...OBBO...",
	"...OO.....OO....",
}

// charSize: frames are 16 wide, 20 tall; drawn 4px above the tile so the head
// overlaps the tile behind, GBA-style.
const (
	charW = 16
	charH = 20
)

// skinPalette is one character look; players get one by skin index.
type skinPalette struct {
	hair, hairShade color.RGBA
	shirt, shirtSh  color.RGBA
	pants           color.RGBA
}

var skinPalettes = []skinPalette{
	{color.RGBA{88, 56, 32, 255}, color.RGBA{64, 40, 24, 255}, color.RGBA{216, 64, 56, 255}, color.RGBA{168, 40, 40, 255}, color.RGBA{56, 80, 144, 255}},       // brown hair, red shirt
	{color.RGBA{32, 32, 40, 255}, color.RGBA{24, 24, 32, 255}, color.RGBA{72, 128, 208, 255}, color.RGBA{48, 96, 168, 255}, color.RGBA{72, 72, 80, 255}},       // black hair, blue shirt
	{color.RGBA{232, 200, 96, 255}, color.RGBA{192, 160, 64, 255}, color.RGBA{72, 160, 96, 255}, color.RGBA{48, 128, 72, 255}, color.RGBA{96, 72, 56, 255}},    // blonde, green shirt
	{color.RGBA{176, 64, 40, 255}, color.RGBA{136, 48, 32, 255}, color.RGBA{232, 232, 232, 255}, color.RGBA{192, 192, 192, 255}, color.RGBA{40, 40, 48, 255}},  // redhead, white shirt
	{color.RGBA{96, 72, 152, 255}, color.RGBA{72, 52, 120, 255}, color.RGBA{248, 216, 96, 255}, color.RGBA{216, 176, 64, 255}, color.RGBA{56, 56, 64, 255}},    // purple hair, gold shirt
	{color.RGBA{72, 136, 144, 255}, color.RGBA{48, 104, 112, 255}, color.RGBA{224, 128, 176, 255}, color.RGBA{192, 96, 144, 255}, color.RGBA{64, 64, 88, 255}}, // teal hair, pink shirt
	{color.RGBA{200, 112, 48, 255}, color.RGBA{160, 84, 32, 255}, color.RGBA{88, 88, 96, 255}, color.RGBA{64, 64, 72, 255}, color.RGBA{112, 88, 56, 255}},      // orange hair, gray shirt
	{color.RGBA{120, 168, 88, 255}, color.RGBA{88, 136, 64, 255}, color.RGBA{144, 88, 200, 255}, color.RGBA{112, 64, 168, 255}, color.RGBA{48, 48, 56, 255}},   // green hair, violet shirt
}

var (
	outlineColor = color.RGBA{24, 24, 24, 255}
	skinColor    = color.RGBA{248, 200, 160, 255}
	eyeColor     = color.RGBA{32, 32, 48, 255}
	bootColor    = color.RGBA{80, 56, 40, 255}
)

func charFrame(art []string, p skinPalette) *ebiten.Image {
	img := ebiten.NewImage(charW, charH)
	for y, row := range art {
		for x, ch := range row {
			var c color.RGBA
			switch ch {
			case 'O':
				c = outlineColor
			case 'H':
				c = p.hair
			case 'h':
				c = p.hairShade
			case 'S':
				c = skinColor
			case 'E':
				c = eyeColor
			case 'C':
				c = p.shirt
			case 'c':
				c = p.shirtSh
			case 'P':
				c = p.pants
			case 'B':
				c = bootColor
			default:
				continue
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// charSet holds one skin's frames: [dir][frame], dir 0 up, 1 right, 2 down,
// 3 left; frame 0 idle, 1 step. Right-facing draws mirror the left frames.
type charSet struct {
	frames  [4][2]*ebiten.Image
	mirrorR bool
}

var charSets []charSet

func loadSprites() {
	tileSprites = map[tileKind]*ebiten.Image{
		tileGrass:     genGrass(),
		tilePath:      genPath(),
		tileTallGrass: genTallGrass(),
		tileFlower:    genFlower(),
		tileWater:     genWater(),
		tileTree:      genTree(),
	}
	charSets = make([]charSet, len(skinPalettes))
	for i, p := range skinPalettes {
		var set charSet
		set.frames[0] = [2]*ebiten.Image{charFrame(charUpIdle, p), charFrame(charUpStep, p)}
		set.frames[2] = [2]*ebiten.Image{charFrame(charDownIdle, p), charFrame(charDownStep, p)}
		set.frames[3] = [2]*ebiten.Image{charFrame(charLeftIdle, p), charFrame(charLeftStep, p)}
		set.frames[1] = set.frames[3] // mirrored at draw time
		set.mirrorR = true
		charSets[i] = set
	}
}

func skinFor(id string) int {
	sum := 0
	for _, b := range []byte(id) {
		sum += int(b)
	}
	return sum % len(skinPalettes)
}
