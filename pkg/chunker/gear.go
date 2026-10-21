package chunker

// gear maps each byte to a pseudo-random 64-bit value. It is generated once from a fixed seed
// (splitmix64), so every build and every node cuts the same content at the same places. Changing
// the seed changes every chunk boundary: treat it as part of the on-disk format.
var gear = func() (t [256]uint64) {
	x := uint64(0x6f6e676769) // "onggi"
	for i := range t {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		t[i] = z ^ z>>31
	}
	return t
}()
