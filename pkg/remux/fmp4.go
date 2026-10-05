package remux

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Splitter is the io.Writer a fragmented-MP4 muxer writes into. It hands
// the init (ftyp+moov) to OnInit once, and each fragment -- any styp/sidx,
// then moof and its mdat -- to OnFragment. Boxes after the last fragment
// (mfra) are dropped.
type Splitter struct {
	OnInit     func(init []byte) error
	OnFragment func(fragment []byte) error

	buf      []byte
	init     []byte
	frag     []byte
	initDone bool
	err      error
}

func (s *Splitter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.buf = append(s.buf, p...)
	for len(s.buf) >= 8 {
		size, hdr := uint64(binary.BigEndian.Uint32(s.buf)), uint64(8)
		if size == 1 {
			if len(s.buf) < 16 {
				break
			}
			size, hdr = binary.BigEndian.Uint64(s.buf[8:]), 16
		}
		if size == 0 {
			s.err = errors.New("fmp4: a box sized to the end of the file")
			return 0, s.err
		}
		if size < hdr {
			s.err = fmt.Errorf("fmp4: box size %d", size)
			return 0, s.err
		}
		if uint64(len(s.buf)) < size {
			break
		}
		b := s.buf[:size]
		if err := s.box(string(b[4:8]), b); err != nil {
			s.err = err
			return 0, err
		}
		s.buf = s.buf[size:]
	}
	return len(p), nil
}

func (s *Splitter) box(typ string, b []byte) error {
	switch typ {
	case "ftyp":
		s.init = append(s.init, b...)
	case "moov":
		s.init = append(s.init, b...)
		s.initDone = true
		return s.OnInit(s.init)
	case "styp", "sidx", "moof":
		s.frag = append(s.frag, b...)
	case "mdat":
		if !s.initDone {
			return errors.New("fmp4: media before the init")
		}
		s.frag = append(s.frag, b...)
		f := s.frag
		s.frag = nil
		return s.OnFragment(f)
	}
	return nil
}
