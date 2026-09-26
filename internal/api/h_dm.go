package api

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/store"
	"github.com/jackgilbert/mockingbird/internal/translate"
	"github.com/jackgilbert/mockingbird/internal/twitter"
)

// Twitter DMs map onto Bluesky chat (chat.bsky.convo.*, proxied through the
// PDS to did:web:api.bsky.chat). Only app passwords created with "Allow
// access to your direct messages" can use chat; others get empty lists.

var errNoDMScope = &APIError{Status: 403, Msg: "This app password cannot access direct messages. Create a new Bluesky app password with \"Allow access to your direct messages\" ticked."}

type dmCollected struct {
	convo string
	msg   atp.ChatMessage
	at    time.Time
	other *atp.Profile
	me    *atp.Profile
}

// collectDMs gathers recent messages across recent conversations. Twitter
// clients want one flat list; Bluesky groups by conversation, so this reads
// the most recent conversations and merges their recent messages.
func (s *Server) collectDMs(c *Ctx, sent bool) ([]dmCollected, error) {
	// One listing costs a listConvos plus a getMessages per conversation, so
	// the merged result is cached briefly per viewer and direction.
	key := c.Sess.DID + "|" + strconv.FormatBool(sent)
	if v, ok := s.dmCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := s.sf.Do("dm:"+key, func() (any, error) {
		out, err := s.collectDMsUncached(c, sent)
		if err == nil {
			s.dmCache.AddTTL(key, out, time.Minute)
		}
		return out, err
	})
	if err != nil {
		return nil, err
	}
	return v.([]dmCollected), nil
}

func (s *Server) collectDMsUncached(c *Ctx, sent bool) ([]dmCollected, error) {
	var convos atp.Convos
	if err := c.Get("chat.bsky.convo.listConvos", url.Values{"limit": {"20"}}, &convos); err != nil {
		return nil, upstreamErr(err)
	}
	var out []dmCollected
	for i, cv := range convos.Convos {
		if i >= 10 {
			break
		}
		var me, other *atp.Profile
		for j := range cv.Members {
			if cv.Members[j].DID == c.Sess.DID {
				me = &cv.Members[j]
			} else if other == nil {
				other = &cv.Members[j]
			}
		}
		if me == nil || other == nil {
			continue // group chats or self-chats have no Twitter equivalent
		}
		var msgs atp.Messages
		if err := c.Get("chat.bsky.convo.getMessages", url.Values{"convoId": {cv.ID}, "limit": {"20"}}, &msgs); err != nil {
			return nil, upstreamErr(err)
		}
		for _, m := range msgs.Messages {
			if m.Deleted() || (m.Sender.DID == c.Sess.DID) != sent {
				continue
			}
			at, _ := atp.ParseTime(m.SentAt)
			out = append(out, dmCollected{convo: cv.ID, msg: m, at: at, other: other, me: me})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out, nil
}

func (s *Server) dmList(c *Ctx, sent bool) (*Resp, error) {
	empty := &Resp{Value: []twitter.DirectMessage{}, Root: "direct-messages", Item: "direct_message"}
	if !c.Sess.CanDM() {
		return empty, nil
	}
	all, err := s.collectDMs(c, sent)
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Status == 403 {
			return empty, nil
		}
		return nil, err
	}
	refs := make([]store.DMRef, len(all))
	for i, m := range all {
		refs[i] = store.DMRef{ConvoID: m.convo, MsgID: m.msg.ID, SortAt: m.at}
	}
	ids, err := s.d.Store.DMIDs(c.Context(), refs)
	if err != nil {
		return nil, err
	}
	var since, maxT time.Time
	if id := c.Int64Arg("since_id"); id > 0 {
		if r, err := s.d.Store.DMByID(c.Context(), id); err == nil {
			since = r.SortAt
		}
	}
	if id := c.Int64Arg("max_id"); id > 0 {
		if r, err := s.d.Store.DMByID(c.Context(), id); err == nil {
			maxT = r.SortAt
		}
	}
	count, page := c.Count(20, 200), c.Page()
	var inputs []translate.DMInput
	skip := (page - 1) * count
	for i := range all {
		m := &all[i]
		if !since.IsZero() && !m.at.After(since) {
			continue
		}
		if !maxT.IsZero() && m.at.After(maxT) {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		in := translate.DMInput{ConvoID: m.convo, Msg: &m.msg, Sender: m.other, Recipient: m.me}
		if sent {
			in.Sender, in.Recipient = m.me, m.other
		}
		inputs = append(inputs, in)
		if len(inputs) == count {
			break
		}
	}
	dms, err := s.builder(c).DirectMessages(c.Context(), inputs, ids)
	if err != nil {
		return nil, err
	}
	if dms == nil {
		dms = []twitter.DirectMessage{}
	}
	return &Resp{Value: dms, Root: "direct-messages", Item: "direct_message", Cacheable: true}, nil
}

func (s *Server) directMessages(c *Ctx) (*Resp, error)     { return s.dmList(c, false) }
func (s *Server) directMessagesSent(c *Ctx) (*Resp, error) { return s.dmList(c, true) }

func (s *Server) directMessageNew(c *Ctx) (*Resp, error) {
	if !c.Sess.CanDM() {
		return nil, errNoDMScope
	}
	ref := c.Arg("user", "user_id", "screen_name")
	if ref == "" {
		return nil, errForbidden("There was an error sending your message: recipient missing.")
	}
	actor, err := c.actor(ref)
	if err != nil {
		return nil, err
	}
	target, err := c.profile(actor)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(translate.UnescapeClientText(c.Form.Get("text")))
	if text == "" {
		return nil, errForbidden("There was an error sending your message: text missing.")
	}
	if len(text) > 10000 || translate.GraphemeCount(text) > 1000 {
		return nil, errForbidden("There was an error sending your message: message is too long.")
	}
	var cv atp.ConvoOutput
	if err := c.Get("chat.bsky.convo.getConvoForMembers", url.Values{"members": {c.Sess.DID, target.DID}}, &cv); err != nil {
		return nil, upstreamErr(err)
	}
	msg := map[string]any{"text": text}
	if fs := translate.BuildFacets(c.Context(), text, s.mentionResolver(c)); len(fs) > 0 {
		msg["facets"] = fs
	}
	s.invalidateDMs(c.Sess.DID, target.DID)
	var sent atp.ChatMessage
	if err := c.Post("chat.bsky.convo.sendMessage", map[string]any{"convoId": cv.Convo.ID, "message": msg}, &sent); err != nil {
		return nil, upstreamErr(err)
	}
	me, err := c.selfProfile()
	if err != nil {
		return nil, err
	}
	at, _ := atp.ParseTime(sent.SentAt)
	ids, err := s.d.Store.DMIDs(c.Context(), []store.DMRef{{ConvoID: cv.Convo.ID, MsgID: sent.ID, SortAt: at}})
	if err != nil {
		return nil, err
	}
	dms, err := s.builder(c).DirectMessages(c.Context(), []translate.DMInput{{ConvoID: cv.Convo.ID, Msg: &sent, Sender: me, Recipient: target}}, ids)
	if err != nil {
		return nil, err
	}
	return &Resp{Value: dms[0], Root: "direct_message"}, nil
}

func (s *Server) directMessageDestroy(c *Ctx) (*Resp, error) {
	if !c.Sess.CanDM() {
		return nil, errNoDMScope
	}
	id := c.Int64Arg("id")
	ref, err := s.d.Store.DMByID(c.Context(), id)
	if err != nil {
		return nil, errNotFound()
	}
	s.invalidateDMs(c.Sess.DID)
	var deleted atp.ChatMessage
	if err := c.Post("chat.bsky.convo.deleteMessageForSelf", map[string]string{"convoId": ref.ConvoID, "messageId": ref.MsgID}, &deleted); err != nil {
		return nil, upstreamErr(err)
	}
	me, err := c.selfProfile()
	if err != nil {
		return nil, err
	}
	// The deleted view carries no text; return a minimal DM so clients can
	// remove it from their list.
	deleted.ID = ref.MsgID
	sender := me
	if deleted.Sender.DID != "" && deleted.Sender.DID != c.Sess.DID {
		if p, err := c.profile(deleted.Sender.DID); err == nil {
			sender = p
		}
	}
	dms, err := s.builder(c).DirectMessages(c.Context(), []translate.DMInput{{ConvoID: ref.ConvoID, Msg: &deleted, Sender: sender, Recipient: me}},
		map[store.DMRef]int64{{ConvoID: ref.ConvoID, MsgID: ref.MsgID}: id})
	if err != nil {
		return nil, err
	}
	return &Resp{Value: dms[0], Root: "direct_message"}, nil
}

// invalidateDMs drops cached DM listings for the given accounts. The other
// party's cache is dropped too when both use this bridge.
func (s *Server) invalidateDMs(dids ...string) {
	for _, d := range dids {
		s.dmCache.Remove(d + "|true")
		s.dmCache.Remove(d + "|false")
	}
}
