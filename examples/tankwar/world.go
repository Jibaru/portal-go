package main

import (
	"math/rand"
)

const (
	tileSize = 16
	tilesX   = 40
	tilesY   = 30
	screenW  = tilesX * tileSize // 640
	screenH  = tilesY * tileSize // 480

	tankSize   = 16
	bulletSize = 4
)

type tileKind uint8

const (
	tileEmpty tileKind = iota
	tileBrick
	tileSteel
)

// world is the shared battlefield. The layout is a fixed function of the tile
// coordinates, so every client renders and collides against the identical map
// with nothing to synchronise.
type world struct {
	tiles [tilesY][tilesX]tileKind
}

func buildWorld() *world {
	w := &world{}
	for y := 0; y < tilesY; y++ {
		for x := 0; x < tilesX; x++ {
			switch {
			case x == 0 || y == 0 || x == tilesX-1 || y == tilesY-1:
				w.tiles[y][x] = tileSteel
			case brickAt(x, y):
				w.tiles[y][x] = tileBrick
			case steelAt(x, y):
				w.tiles[y][x] = tileSteel
			}
		}
	}
	return w
}

// brickAt draws Battle City-style vertical brick pillars with two fighting
// lanes, plus short horizontal baffles in the middle band.
func brickAt(x, y int) bool {
	pillar := (x >= 5 && x <= 7) || (x >= 12 && x <= 14) || (x >= 19 && x <= 21) ||
		(x >= 25 && x <= 27) || (x >= 32 && x <= 34)
	if pillar && ((y >= 4 && y <= 10) || (y >= 19 && y <= 25)) {
		return true
	}
	baffle := (y == 14 || y == 15) &&
		((x >= 8 && x <= 10) || (x >= 16 && x <= 17) || (x >= 22 && x <= 23) || (x >= 29 && x <= 31))
	return baffle
}

// steelAt sprinkles a few indestructible anchors.
func steelAt(x, y int) bool {
	if (x == 19 || x == 20) && (y == 14 || y == 15) {
		return true
	}
	return (x == 9 || x == 30) && (y == 7 || y == 22)
}

func (w *world) solidAt(tx, ty int) bool {
	if tx < 0 || ty < 0 || tx >= tilesX || ty >= tilesY {
		return true
	}
	return w.tiles[ty][tx] != tileEmpty
}

// boxCollides reports whether the pixel-space box (x, y, size, size) overlaps
// any solid tile.
func (w *world) boxCollides(x, y float64, size int) bool {
	x0 := int(x) / tileSize
	y0 := int(y) / tileSize
	x1 := (int(x) + size - 1) / tileSize
	y1 := (int(y) + size - 1) / tileSize
	for ty := y0; ty <= y1; ty++ {
		for tx := x0; tx <= x1; tx++ {
			if w.solidAt(tx, ty) {
				return true
			}
		}
	}
	return false
}

// randomSpawn finds a free spot with some clearance from other tanks.
func (w *world) randomSpawn(occupied [][2]float64) (float64, float64) {
	for try := 0; try < 200; try++ {
		x := float64(tileSize + rand.Intn(screenW-3*tileSize))
		y := float64(tileSize + rand.Intn(screenH-3*tileSize))
		if w.boxCollides(x, y, tankSize) {
			continue
		}
		clear := true
		for _, o := range occupied {
			dx, dy := x-o[0], y-o[1]
			if dx*dx+dy*dy < 96*96 {
				clear = false
				break
			}
		}
		if clear {
			return x, y
		}
	}
	// Fall back to a corner lane if the field is crowded.
	return float64(2 * tileSize), float64(2 * tileSize)
}

// Directions: 0 up, 1 right, 2 down, 3 left — Battle City's four ways.
func dirVector(dir int) (float64, float64) {
	switch dir {
	case 0:
		return 0, -1
	case 1:
		return 1, 0
	case 2:
		return 0, 1
	default:
		return -1, 0
	}
}
