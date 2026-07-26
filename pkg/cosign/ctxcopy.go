package cosign

import (
	"context"
	"io"
)

// ctxCopy does an io.Copy with periodic ctx cancellation checks
// between Read calls. io.Copy itself doesn't check ctx, and
// streaming a 1-2 GB layer through SHA-256 is exactly the kind of
// unbounded work that would happily finish after a caller
// cancelled. Polling every 256 KiB bounds the worst-case
// overshoot to a single block and adds negligible overhead.
//
// This is intentionally not exported (lowercase) — it's a
// narrow-purpose helper for Sign's streamed SHA-256, with no
// reason to be part of pkg/cosign's public surface. Promote if
// the verifier or other consumers also need ctx-aware reads.
type ctxCopy struct {
	dst io.Writer
	src io.Reader
	buf []byte
	ctx context.Context
}

func newCtxCopy(dst io.Writer, src io.Reader, ctx context.Context) *ctxCopy {
	return &ctxCopy{
		dst: dst,
		src: src,
		buf: make([]byte, 256*1024),
		ctx: ctx,
	}
}

func (c *ctxCopy) Do() (int64, error) {
	var total int64
	for {
		if err := c.ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := c.src.Read(c.buf)
		if n > 0 {
			w, werr := c.dst.Write(c.buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w < n {
				return total, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
