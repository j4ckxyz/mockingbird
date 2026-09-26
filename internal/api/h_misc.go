package api

import (
	"errors"
	"strings"

	"github.com/j4ckxyz/mockingbird/internal/store"
	"github.com/j4ckxyz/mockingbird/internal/twitter"
)

// help/test returns "ok" in JSON and <ok>true</ok> in XML, as Twitter did.
func (s *Server) helpTest(c *Ctx) (*Resp, error) {
	if c.Format == "xml" {
		return &Resp{Root: "ok", Value: true}, nil
	}
	return &Resp{Value: "ok"}, nil
}

func savedSearch(ss store.SavedSearch) twitter.SavedSearch {
	return twitter.SavedSearch{ID: ss.ID, Name: ss.Query, Query: ss.Query, CreatedAt: twitter.Time(ss.CreatedAt), IDStr: twitter.IDString(ss.ID)}
}

func (s *Server) savedSearches(c *Ctx) (*Resp, error) {
	list, err := s.d.Store.SavedSearches(c.Context(), c.Sess.DID)
	if err != nil {
		return nil, err
	}
	out := make([]twitter.SavedSearch, 0, len(list))
	for _, ss := range list {
		out = append(out, savedSearch(ss))
	}
	return &Resp{Value: out, Root: "saved_searches", Item: "saved_search"}, nil
}

func (s *Server) savedSearchShow(c *Ctx) (*Resp, error) {
	ss, err := s.d.Store.SavedSearch(c.Context(), c.Sess.DID, c.Int64Arg("id"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNotFound()
	} else if err != nil {
		return nil, err
	}
	return &Resp{Value: savedSearch(ss), Root: "saved_search"}, nil
}

func (s *Server) savedSearchCreate(c *Ctx) (*Resp, error) {
	q := strings.TrimSpace(c.Form.Get("query"))
	if q == "" || len(q) > 500 {
		return nil, errForbidden("Query is required.")
	}
	ss, err := s.d.Store.CreateSavedSearch(c.Context(), c.Sess.DID, q, s.now())
	if err != nil {
		return nil, errForbidden("You have reached the limit of saved searches.")
	}
	return &Resp{Value: savedSearch(ss), Root: "saved_search"}, nil
}

func (s *Server) savedSearchDestroy(c *Ctx) (*Resp, error) {
	ss, err := s.d.Store.DeleteSavedSearch(c.Context(), c.Sess.DID, c.Int64Arg("id"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNotFound()
	} else if err != nil {
		return nil, err
	}
	return &Resp{Value: savedSearch(ss), Root: "saved_search"}, nil
}
