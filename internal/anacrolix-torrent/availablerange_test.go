package torrent

import (
	"testing"

	"github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"

	pp "github.com/anacrolix/torrent/peer_protocol"
)

// AvailableRange must never credit bytes past the end of a complete piece: the reader
// takes the count as a read length, and the bytes beyond belong to the next piece, which
// may still be filling. MemPiece serves those from a recycled buffer, so they arrive as
// plausible-looking video data rather than as an error.
func TestAvailableRangeStopsAtAnIncompletePiece(t *testing.T) {
	const pieceLen = 4 << 20
	const chunkSize = 16 << 10

	tor := &Torrent{
		info:      &metainfo.Info{PieceLength: pieceLen, Length: 3 * pieceLen},
		_length:   generics.Some[int64](3 * pieceLen),
		chunkSize: pp.Integer(chunkSize),
	}
	tor._completedPieces.Add(0) // piece 0 done, pieces 1 and 2 still downloading

	// Read from the middle of piece 0, far enough in that off's chunk is not the first:
	// that gap is exactly what the old code credited twice.
	const off = pieceLen / 2
	got := tor.AvailableRange(off, 3*pieceLen, false)
	want := int64(pieceLen - off)

	if got != want {
		t.Fatalf("AvailableRange(%d) = %d, want %d (over-credited %d bytes of piece 1)",
			off, got, want, got-want)
	}
}

// An offset already chunk-aligned at the piece start is the one case the old arithmetic
// got right; keep it covered so a fix to the general case does not break it.
func TestAvailableRangeAtPieceStart(t *testing.T) {
	const pieceLen = 4 << 20
	const chunkSize = 16 << 10

	tor := &Torrent{
		info:      &metainfo.Info{PieceLength: pieceLen, Length: 2 * pieceLen},
		_length:   generics.Some[int64](2 * pieceLen),
		chunkSize: pp.Integer(chunkSize),
	}
	tor._completedPieces.Add(0)

	if got, want := tor.AvailableRange(0, 2*pieceLen, false), int64(pieceLen); got != want {
		t.Fatalf("AvailableRange(0) = %d, want %d", got, want)
	}
}

// Two complete pieces in a row must accumulate across the boundary, so the fix does not
// turn every piece edge into a stall.
func TestAvailableRangeSpansCompletePieces(t *testing.T) {
	const pieceLen = 4 << 20
	const chunkSize = 16 << 10

	tor := &Torrent{
		info:      &metainfo.Info{PieceLength: pieceLen, Length: 3 * pieceLen},
		_length:   generics.Some[int64](3 * pieceLen),
		chunkSize: pp.Integer(chunkSize),
	}
	tor._completedPieces.Add(0)
	tor._completedPieces.Add(1)

	const off = pieceLen / 2
	if got, want := tor.AvailableRange(off, 3*pieceLen, false), int64(2*pieceLen-off); got != want {
		t.Fatalf("AvailableRange(%d) = %d, want %d", off, got, want)
	}
}
