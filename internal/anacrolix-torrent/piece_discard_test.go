package torrent

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	pp "github.com/anacrolix/torrent/peer_protocol"
)

// A partial piece may only be freed together with its dirty chunks, and never while something
// still wants it or a chunk write is running: either way the next hash check fails and the peers
// that sent the chunks are blamed.
func TestDiscardPartial(t *testing.T) {
	const pieceLen, chunkSize = 512 << 10, 16 << 10

	setup := func() (*Torrent, *Piece) {
		tor := &Torrent{
			cl:                     &Client{},
			info:                   &metainfo.Info{PieceLength: pieceLen, Length: 3 * pieceLen},
			chunkSize:              pp.Integer(chunkSize),
			_chunksPerRegularPiece: pieceLen / chunkSize,
		}
		tor.pieces = make([]Piece, 3)
		for i := range tor.pieces {
			tor.pieces[i].t, tor.pieces[i].index = tor, i
		}
		p := &tor.pieces[1]
		for i := chunkIndexType(0); i < 4; i++ {
			p.unpendChunkIndex(i)
		}
		return tor, p
	}

	refused := map[string]func(*Torrent, *Piece){
		"wanted":   func(_ *Torrent, p *Piece) { p.priority = PiecePriorityNormal },
		"writing":  func(_ *Torrent, p *Piece) { p.pendingWrites = 1 },
		"complete": func(tor *Torrent, p *Piece) { tor._completedPieces.Add(uint32(p.index)) },
		"hashing":  func(_ *Torrent, p *Piece) { p.hashing = true },
	}
	for name, arrange := range refused {
		tor, p := setup()
		arrange(tor, p)
		dropped := false
		if p.discardPartialLocked(func() { dropped = true }) || dropped || p.numDirtyChunks() != 4 {
			t.Errorf("%s: piece discarded (dropped=%v, dirty chunks %d of 4)", name, dropped, p.numDirtyChunks())
		}
	}

	tor, p := setup()
	dropped := false
	if !p.discardPartialLocked(func() { dropped = true }) || !dropped {
		t.Fatalf("unwanted partial piece kept (dropped=%v)", dropped)
	}
	if n := p.numDirtyChunks(); n != 0 {
		t.Fatalf("%d chunks still dirty after the storage was dropped", n)
	}
	if n := tor.pieces[0].numDirtyChunks() + tor.pieces[2].numDirtyChunks(); n != 0 {
		t.Fatalf("neighbouring pieces lost or gained %d dirty chunks", n)
	}
}
