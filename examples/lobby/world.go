package main

import "math/rand"

const (
	tileSize = 16
	tilesX   = 40
	tilesY   = 30
	screenW  = tilesX * tileSize // 640
	screenH  = tilesY * tileSize // 480

	// The player's collision box (feet area), smaller than the sprite.
	bodyW = 12
	bodyH = 10
)

type tileKind uint8

const (
	tileGrass tileKind = iota
	tilePath
	tileTallGrass
	tileFlower
	tileWater
	tileTree
)

// world is the lobby plaza. Layout is a pure function of coordinates — every
// client renders and collides against the identical map with nothing synced.
type world struct {
	tiles [tilesY][tilesX]tileKind
}

func buildWorld() *world {
	w := &world{}
	for y := 0; y < tilesY; y++ {
		for x := 0; x < tilesX; x++ {
			w.tiles[y][x] = tileAt(x, y)
		}
	}
	return w
}

func tileAt(x, y int) tileKind {
	// Tree ring border.
	if x == 0 || y == 0 || x == tilesX-1 || y == tilesY-1 {
		return tileTree
	}
	// A pond, upper right.
	if x >= 28 && x <= 35 && y >= 4 && y <= 9 {
		return tileWater
	}
	// Crossing paths.
	if y == 15 || y == 16 || ((x == 19 || x == 20) && y >= 1 && y <= tilesY-2) {
		return tilePath
	}
	// A tall-grass patch, upper left.
	if x >= 3 && x <= 9 && y >= 3 && y <= 8 {
		return tileTallGrass
	}
	// A few inner trees.
	if (x == 12 && y == 22) || (x == 27 && y == 21) || (x == 6 && y == 24) || (x == 33 && y == 13) {
		return tileTree
	}
	// Scattered flowers.
	if (x*7+y*13)%29 == 0 {
		return tileFlower
	}
	return tileGrass
}

func (w *world) solid(tx, ty int) bool {
	if tx < 0 || ty < 0 || tx >= tilesX || ty >= tilesY {
		return true
	}
	k := w.tiles[ty][tx]
	return k == tileTree || k == tileWater
}

// boxCollides checks the feet box at world position (x, y) — x,y is the
// sprite's top-left; the feet box sits at the sprite's lower half.
func (w *world) boxCollides(x, y float64) bool {
	left := int(x) + (charW-bodyW)/2
	top := int(y) + charH - bodyH
	for ty := top / tileSize; ty <= (top+bodyH-1)/tileSize; ty++ {
		for tx := left / tileSize; tx <= (left+bodyW-1)/tileSize; tx++ {
			if w.solid(tx, ty) {
				return true
			}
		}
	}
	return false
}

// randomSpawn picks a free spot near the path crossing.
func (w *world) randomSpawn() (float64, float64) {
	for try := 0; try < 200; try++ {
		x := float64((14 + rand.Intn(12)) * tileSize)
		y := float64((12 + rand.Intn(6)) * tileSize)
		if !w.boxCollides(x, y) {
			return x, y
		}
	}
	return float64(19 * tileSize), float64(14 * tileSize)
}

// Directions: 0 up, 1 right, 2 down, 3 left.
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
