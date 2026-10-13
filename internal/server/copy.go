package server

import (
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/transfer"
)

type copyReq struct {
	Items []farewell.Item `json:"items"`
	To    farewell.Item   `json:"to"` // the folder to copy into: a disk, and a path on it ("" is the root of the disk)
}

func (s *Server) copier(job *Job) transfer.Copier {
	c := transfer.Copier{Catalog: s.catalog(), Guard: s.guardPath}
	if job != nil {
		c.Progress = job
	}
	return c
}

// doCopy copies files and folders from any disk to a folder on any disk (the same one too).
func (s *Server) doCopy(req copyReq, job *Job) (any, error) {
	if len(req.Items) == 0 {
		return nil, refuse("nothing to copy")
	}
	sel, err := s.box(job).Check(req.Items, "copied")
	if err != nil {
		return nil, err
	}
	defer sel.Close()
	items, from := transfer.FromDisks(sel)
	return s.copier(job).Copy(items, from, req.To)
}

// doPhoneCopy copies files from an Android phone to a folder on a disk. The phone is not touched.
func (s *Server) doPhoneCopy(req copyReq, job *Job) (any, error) {
	label, err := oneSource(req.Items)
	if err != nil {
		return nil, err
	}
	psrc, dev, err := s.phoneDevice(label)
	if err != nil {
		return nil, err
	}
	pdb, err := idx.Open(psrc.DB)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	items, err := transfer.PhoneItems(pdb, psrc, req.Items)
	if err != nil {
		return nil, err
	}
	return s.copier(job).Copy(items, transfer.FromPhone(pdb, dev.Serial), req.To)
}
