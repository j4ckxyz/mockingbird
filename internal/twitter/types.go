// Package twitter defines the 2009-2010 Twitter API v1 response shapes and
// their JSON, XML, JSONP, RSS and Atom encodings.
//
// Field order follows Twitter's XML documents of the period. JSON parsers do
// not care about order, but a few event-driven XML parsers in old clients
// assume it. Nullable fields are pointers without omitempty, because Twitter
// always emitted them (as null in JSON and as empty elements in XML).
package twitter

import (
	"strconv"
	"time"
)

// Time is a timestamp rendered in Twitter's REST format,
// "Wed Aug 27 13:08:45 +0000 2008", in JSON and XML.
type Time time.Time

// RESTTimeLayout is Twitter's REST API date layout.
const RESTTimeLayout = "Mon Jan 02 15:04:05 -0700 2006"

// SearchTimeLayout is the Search API's RFC 822 style layout.
const SearchTimeLayout = "Mon, 02 Jan 2006 15:04:05 -0700"

// String formats t in UTC with Twitter's layout.
func (t Time) String() string { return time.Time(t).UTC().Format(RESTTimeLayout) }

// MarshalJSON renders the quoted REST format.
func (t Time) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(t.String())), nil }

// SearchTime renders in the Search API format.
type SearchTime time.Time

// String formats t in UTC with the Search API layout.
func (t SearchTime) String() string { return time.Time(t).UTC().Format(SearchTimeLayout) }

// MarshalJSON renders the quoted Search format.
func (t SearchTime) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(t.String())), nil }

// User is a Twitter user object. Element order follows real mid-2010 API
// output archived on dev.twitter.com; fields added later in 2010 are
// appended before the nested status.
type User struct {
	ID                        int64   `json:"id"`
	Name                      string  `json:"name"`
	ScreenName                string  `json:"screen_name"`
	Location                  string  `json:"location"`
	Description               string  `json:"description"`
	ProfileImageURL           string  `json:"profile_image_url"`
	URL                       *string `json:"url"`
	Protected                 bool    `json:"protected"`
	FollowersCount            int64   `json:"followers_count"`
	ProfileBackgroundColor    string  `json:"profile_background_color"`
	ProfileTextColor          string  `json:"profile_text_color"`
	ProfileLinkColor          string  `json:"profile_link_color"`
	ProfileSidebarFillColor   string  `json:"profile_sidebar_fill_color"`
	ProfileSidebarBorderColor string  `json:"profile_sidebar_border_color"`
	FriendsCount              int64   `json:"friends_count"`
	CreatedAt                 Time    `json:"created_at"`
	FavouritesCount           int64   `json:"favourites_count"`
	UTCOffset                 *int    `json:"utc_offset"`
	TimeZone                  *string `json:"time_zone"`
	ProfileBackgroundImageURL string  `json:"profile_background_image_url"`
	ProfileBackgroundTile     bool    `json:"profile_background_tile"`
	ProfileUseBackgroundImage bool    `json:"profile_use_background_image"`
	Notifications             *bool   `json:"notifications"`
	GeoEnabled                bool    `json:"geo_enabled"`
	Verified                  bool    `json:"verified"`
	Following                 *bool   `json:"following"`
	StatusesCount             int64   `json:"statuses_count"`
	Lang                      string  `json:"lang"`
	ContributorsEnabled       bool    `json:"contributors_enabled"`
	FollowRequestSent         *bool   `json:"follow_request_sent"`
	ListedCount               int64   `json:"listed_count"`
	ShowAllInlineMedia        bool    `json:"show_all_inline_media"`
	IDStr                     string  `json:"id_str" xml:"-"`
	Status                    *Status `json:"status,omitempty" xml:",omitempty"`
}

// Status is a Twitter status (tweet). In XML, retweeted_status comes before
// user, as in the statuses/retweet documentation.
type Status struct {
	CreatedAt            Time    `json:"created_at"`
	ID                   int64   `json:"id"`
	Text                 string  `json:"text"`
	Source               string  `json:"source"`
	Truncated            bool    `json:"truncated"`
	InReplyToStatusID    *int64  `json:"in_reply_to_status_id"`
	InReplyToUserID      *int64  `json:"in_reply_to_user_id"`
	Favorited            bool    `json:"favorited"`
	InReplyToScreenName  *string `json:"in_reply_to_screen_name"`
	RetweetedStatus      *Status `json:"retweeted_status,omitempty" xml:",omitempty"`
	User                 *User   `json:"user,omitempty" xml:",omitempty"`
	Geo                  any     `json:"geo"`
	Coordinates          any     `json:"coordinates"`
	Place                any     `json:"place"`
	Contributors         any     `json:"contributors"`
	RetweetCount         int64   `json:"retweet_count"`
	Retweeted            bool    `json:"retweeted"`
	IDStr                string  `json:"id_str" xml:"-"`
	InReplyToStatusIDStr *string `json:"in_reply_to_status_id_str" xml:"-"`
	InReplyToUserIDStr   *string `json:"in_reply_to_user_id_str" xml:"-"`
}

// DirectMessage is a Twitter direct message.
type DirectMessage struct {
	ID                  int64  `json:"id"`
	SenderID            int64  `json:"sender_id"`
	Text                string `json:"text"`
	RecipientID         int64  `json:"recipient_id"`
	CreatedAt           Time   `json:"created_at"`
	SenderScreenName    string `json:"sender_screen_name"`
	RecipientScreenName string `json:"recipient_screen_name"`
	Sender              *User  `json:"sender"`
	Recipient           *User  `json:"recipient"`
	IDStr               string `json:"id_str" xml:"-"`
}

// SearchResult is one Search API result. Its shape is distinct from Status.
type SearchResult struct {
	Text            string         `json:"text"`
	ToUserID        *int64         `json:"to_user_id"`
	ToUser          *string        `json:"to_user,omitempty"`
	FromUser        string         `json:"from_user"`
	Metadata        SearchMetadata `json:"metadata"`
	ID              int64          `json:"id"`
	FromUserID      int64          `json:"from_user_id"`
	ISOLanguageCode string         `json:"iso_language_code"`
	Source          string         `json:"source"`
	ProfileImageURL string         `json:"profile_image_url"`
	CreatedAt       SearchTime     `json:"created_at"`
	Geo             any            `json:"geo"`
	IDStr           string         `json:"id_str"`
	FromUserIDStr   string         `json:"from_user_id_str"`
	ToUserIDStr     *string        `json:"to_user_id_str,omitempty"`
}

// SearchMetadata is the per-result metadata node.
type SearchMetadata struct {
	ResultType string `json:"result_type"`
}

// SearchResponse is the Search API envelope.
type SearchResponse struct {
	Results        []SearchResult `json:"results"`
	MaxID          int64          `json:"max_id"`
	SinceID        int64          `json:"since_id"`
	RefreshURL     string         `json:"refresh_url"`
	NextPage       string         `json:"next_page,omitempty"`
	ResultsPerPage int            `json:"results_per_page"`
	Page           int            `json:"page"`
	CompletedIn    float64        `json:"completed_in"`
	WarningText    string         `json:"warning,omitempty"`
	Query          string         `json:"query"`
	MaxIDStr       string         `json:"max_id_str"`
	SinceIDStr     string         `json:"since_id_str"`
}

// RelationshipSide is one side of a friendships/show relationship.
type RelationshipSide struct {
	ID                   int64  `json:"id"`
	ScreenName           string `json:"screen_name"`
	Following            bool   `json:"following"`
	FollowedBy           bool   `json:"followed_by"`
	NotificationsEnabled *bool  `json:"notifications_enabled,omitempty" xml:",omitempty"`
	Blocking             *bool  `json:"blocking,omitempty" xml:",omitempty"`
	MarkedSpam           *bool  `json:"marked_spam,omitempty" xml:",omitempty"`
	WantRetweets         *bool  `json:"want_retweets,omitempty" xml:",omitempty"`
	AllReplies           *bool  `json:"all_replies,omitempty" xml:",omitempty"`
	IDStr                string `json:"id_str" xml:"-"`
}

// Relationship is the friendships/show payload.
type Relationship struct {
	Relationship struct {
		Source RelationshipSide `json:"source"`
		Target RelationshipSide `json:"target"`
	} `json:"relationship"`
}

// SavedSearch is a saved search.
type SavedSearch struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	Position  *int   `json:"position"`
	CreatedAt Time   `json:"created_at"`
	IDStr     string `json:"id_str" xml:"-"`
}

// RateLimitStatus is account/rate_limit_status. Its XML form uses dashed
// element names with type attributes, so it has a custom XML encoding.
type RateLimitStatus struct {
	RemainingHits      int   `json:"remaining_hits"`
	HourlyLimit        int   `json:"hourly_limit"`
	ResetTimeInSeconds int64 `json:"reset_time_in_seconds"`
	ResetTime          Time  `json:"reset_time"`
}

// ErrorBody is Twitter's error payload.
type ErrorBody struct {
	Request string `json:"request"`
	Error   string `json:"error"`
}

// IDList is a cursored friends/ids or followers/ids response.
type IDList struct {
	IDs               []int64 `json:"ids" xml:"ids,item=id"`
	NextCursor        int64   `json:"next_cursor"`
	PreviousCursor    int64   `json:"previous_cursor"`
	NextCursorStr     string  `json:"next_cursor_str" xml:"-"`
	PreviousCursorStr string  `json:"previous_cursor_str" xml:"-"`
}

// UserList is a cursored statuses/friends or statuses/followers response.
type UserList struct {
	Users             []User `json:"users" xml:"users,item=user,array"`
	NextCursor        int64  `json:"next_cursor"`
	PreviousCursor    int64  `json:"previous_cursor"`
	NextCursorStr     string `json:"next_cursor_str" xml:"-"`
	PreviousCursorStr string `json:"previous_cursor_str" xml:"-"`
}

// Trend is one trend in the Search API trends.json.
type Trend struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// TrendQuery is one trend in trends/current.json.
type TrendQuery struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// IDString formats an ID for the *_str fields.
func IDString(id int64) string { return strconv.FormatInt(id, 10) }
