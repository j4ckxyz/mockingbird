package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/media"
	"github.com/jackgilbert/mockingbird/internal/secret"
	"github.com/jackgilbert/mockingbird/internal/session"
	"github.com/jackgilbert/mockingbird/internal/store"
	"github.com/jackgilbert/mockingbird/internal/twitter"
)

// TwitPic-compatible image upload.
//
// Clients with a configurable image service POST multipart/form-data with a
// "media" file plus either username/password fields (TwitPic API v1), Basic
// Auth, or OAuth Echo headers. The image is uploaded as a blob to the user's
// PDS and a bridge URL is returned. When a later status contains that URL,
// statuses/update removes it and attaches the image embed instead.

const maxBlobBytes = 950 << 10 // Bluesky's image limit is 1,000,000 bytes

type twitpicResult struct {
	MediaID  string
	MediaURL string
	StatusID int64
	UserID   int64
	Width    int
	Height   int
	Size     int
	Text     string
	User     string
}

func (s *Server) twitpicAuth(c *Ctx) (*session.Session, error) {
	if c.Sess != nil {
		return c.Sess, nil
	}
	// OAuth Echo: the client forwards the Authorization header it would send
	// to verify_credentials.
	if echo := c.r.Header.Get("X-Verify-Credentials-Authorization"); echo != "" {
		if len(echo) > 6 && strings.EqualFold(echo[:6], "oauth ") {
			return s.oauthSession(c, parseOAuthHeader(echo))
		}
	}
	user, pass := c.r.FormValue("username"), c.r.FormValue("password")
	if user == "" {
		return nil, errUnauthorized()
	}
	return s.d.Sessions.Basic(c.Context(), c.IP, user, pass)
}

func (s *Server) handleUpload(c *Ctx, post bool) (*Resp, error) {
	if err := c.r.ParseMultipartForm(1 << 20); err != nil {
		return s.twitpicError(c, 1004, "Invalid or missing image"), nil
	}
	defer c.r.MultipartForm.RemoveAll()
	sess, err := s.twitpicAuth(c)
	if err != nil {
		return s.twitpicError(c, 1001, "Invalid twitter username or password"), nil
	}
	c.Sess = sess
	f, _, err := c.r.FormFile("media")
	if err != nil {
		return s.twitpicError(c, 1002, "Image not found"), nil
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 5<<20))
	if err != nil {
		return s.twitpicError(c, 1004, "Invalid or missing image"), nil
	}
	body, w, h, err := prepareUpload(raw)
	if err != nil {
		return s.twitpicError(c, 1003, "Invalid file type"), nil
	}
	var out atp.UploadBlobOutput
	if err := sess.Do(c.Context(), &atp.Request{Method: http.MethodPost, NSID: "com.atproto.repo.uploadBlob",
		Body: bytes.NewReader(body), ContentType: "image/jpeg"}, &out); err != nil {
		return nil, upstreamErr(err)
	}
	tok := secret.Token(16)
	if err := s.d.Store.PutUpload(c.Context(), &store.Upload{Token: tok, DID: sess.DID, BlobJSON: string(out.Blob),
		MIME: "image/jpeg", Width: w, Height: h, CreatedAt: s.now()}); err != nil {
		return nil, err
	}
	res := twitpicResult{MediaID: tok, MediaURL: s.cfg.PublicURL.String() + "/m/" + tok, Width: w, Height: h, Size: len(body),
		Text: c.r.FormValue("message"), User: sess.Handle()}
	uid, _ := s.builder(c).UserID(c.Context(), sess.DID, sess.Handle())
	res.UserID = uid
	if post {
		text := strings.TrimSpace(res.Text + " " + res.MediaURL)
		c.Form.Set("status", text)
		c.Form.Del("in_reply_to_status_id")
		r, err := s.updateStatus(c)
		if err != nil {
			return nil, err
		}
		if st, ok := r.Value.(*twitter.Status); ok {
			res.StatusID = st.ID
		}
	}
	return s.twitpicOK(c, res), nil
}

// prepareUpload normalises an uploaded image to a JPEG under the blob limit.
func prepareUpload(raw []byte) ([]byte, int, int, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, err
	}
	if format == "jpeg" && len(raw) <= maxBlobBytes {
		return raw, cfg.Width, cfg.Height, nil
	}
	for _, w := range []int{2000, 1600, 1200, 800} {
		out, err := media.Render(raw, min(w, cfg.Width), false)
		if err != nil {
			return nil, 0, 0, err
		}
		if len(out) <= maxBlobBytes {
			c, err := jpeg.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				return nil, 0, 0, err
			}
			return out, c.Width, c.Height, nil
		}
	}
	return nil, 0, 0, errors.New("image too large")
}

func (s *Server) twitpicUpload(c *Ctx) (*Resp, error)        { return s.handleUpload(c, false) }
func (s *Server) twitpicUploadAndPost(c *Ctx) (*Resp, error) { return s.handleUpload(c, true) }

func (s *Server) twitpicOK(c *Ctx, r twitpicResult) *Resp {
	if c.Format == "json" && (strings.HasPrefix(c.Path, "2/") || strings.HasSuffix(c.r.URL.Path, ".json")) {
		b, _ := json.Marshal(map[string]any{
			"id": r.MediaID, "text": r.Text, "url": r.MediaURL, "width": r.Width, "height": r.Height, "size": r.Size,
			"type": "jpg", "timestamp": s.now().UTC().Format(twitter.RESTTimeLayout),
			"user": map[string]any{"id": r.UserID, "screen_name": r.User},
		})
		return &Resp{Raw: b, ContentType: "application/json; charset=utf-8"}
	}
	var w twitter.XMLWriter
	w.Header()
	w.Open("rsp", "stat", "ok")
	if r.StatusID != 0 {
		w.Elem("statusid", strconv.FormatInt(r.StatusID, 10))
		w.Elem("userid", strconv.FormatInt(r.UserID, 10))
	}
	w.Elem("mediaid", r.MediaID)
	w.Elem("mediaurl", r.MediaURL)
	w.Close("rsp")
	return &Resp{Raw: w.Bytes(), ContentType: "application/xml; charset=utf-8"}
}

func (s *Server) twitpicError(c *Ctx, code int, msg string) *Resp {
	var w twitter.XMLWriter
	w.Header()
	w.WriteString(`<rsp stat="fail"><err code="` + strconv.Itoa(code) + `" msg="`)
	w.Text(msg)
	w.WriteString(`" /></rsp>`)
	return &Resp{Raw: w.Bytes(), ContentType: "application/xml; charset=utf-8", Status: 401}
}

// ServeUpload renders /m/<token>: the uploaded image, fetched from the
// owner's PDS and cached through the image pipeline.
func (s *Server) ServeUpload(w http.ResponseWriter, r *http.Request, token string) {
	up, err := s.d.Store.GetUpload(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var blob atp.Blob
	if json.Unmarshal([]byte(up.BlobJSON), &blob) != nil || blob.Ref.Link == "" {
		http.NotFound(w, r)
		return
	}
	key := "upload/" + token
	if b, ok := s.d.Images.Cache.Get(key); ok {
		writeImage(w, r, b)
		return
	}
	who, err := s.d.Resolver.Resolve(r.Context(), up.DID)
	if err != nil {
		http.Error(w, "unavailable", http.StatusBadGateway)
		return
	}
	var buf bytes.Buffer
	err = s.d.Sessions.Client(who.PDS).Do(r.Context(), &atp.Request{NSID: "com.atproto.sync.getBlob",
		Params: queryValues("did", up.DID, "cid", blob.Ref.Link)}, &buf)
	if err != nil {
		http.Error(w, "unavailable", http.StatusBadGateway)
		return
	}
	out, err := media.Render(buf.Bytes(), 480, false)
	if err != nil {
		http.Error(w, "unavailable", http.StatusBadGateway)
		return
	}
	s.d.Images.Cache.Put(key, out)
	writeImage(w, r, out)
}

func writeImage(w http.ResponseWriter, r *http.Request, b []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}
