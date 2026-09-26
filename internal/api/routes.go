package api

// buildRoutes lists every endpoint. Patterns are matched segment by segment
// after the version prefix and format extension are stripped.
func (s *Server) buildRoutes() []*route {
	s.routes = nil
	const (
		jx    = "json xml"
		feeds = "json xml rss atom"
		tl    = "count page since_id max_id id user_id screen_name"
	)
	R, O, N := authRequired, authOptional, authNone

	// Timelines
	s.add("GET", "statuses/home_timeline", R, feeds, s.homeTimeline, tl)
	s.add("GET", "statuses/friends_timeline", R, feeds, s.homeTimeline, tl)
	s.add("GET", "statuses/friends_timeline/:id", R, feeds, s.homeTimeline, tl)
	s.add("GET", "statuses/user_timeline", O, feeds, s.userTimeline, tl)
	s.add("GET", "statuses/user_timeline/:id", O, feeds, s.userTimeline, tl)
	s.add("GET", "statuses/mentions", R, feeds, s.mentions, tl)
	s.add("GET", "statuses/replies", R, feeds, s.mentions, tl)
	s.add("GET", "statuses/public_timeline", O, feeds, s.publicTimeline, tl)
	s.add("GET", "statuses/retweeted_by_me", R, feeds, s.retweetedByMe, tl)
	s.add("GET", "statuses/retweeted_to_me", R, feeds, s.retweetedToMe, tl)
	s.add("GET", "statuses/retweets_of_me", R, feeds, s.retweetsOfMe, tl)
	s.add("GET", "favorites", R, feeds, s.favorites, tl)
	s.add("GET", "favorites/:id", R, feeds, s.favorites, tl)

	// Statuses
	s.add("GET", "statuses/show", O, jx, s.showStatus, "id")
	s.add("GET", "statuses/show/:id", O, jx, s.showStatus, "id")
	s.add("POST", "statuses/update", R, jx, s.updateStatus, "status in_reply_to_status_id lat long display_coordinates place_id media_ids")
	s.add("POST", "statuses/destroy", R, jx, s.destroyStatus, "id")
	s.add("POST", "statuses/destroy/:id", R, jx, s.destroyStatus, "id")
	s.add("DELETE", "statuses/destroy/:id", R, jx, s.destroyStatus, "id")
	s.add("POST", "statuses/retweet/:id", R, jx, s.retweet, "id")
	s.add("PUT", "statuses/retweet/:id", R, jx, s.retweet, "id")
	s.add("GET", "statuses/retweets/:id", O, jx, s.retweets, "id count")
	s.add("POST", "favorites/create/:id", R, jx, s.favoriteCreate, "id")
	s.add("POST", "favorites/create", R, jx, s.favoriteCreate, "id")
	s.add("POST", "favorites/destroy/:id", R, jx, s.favoriteDestroy, "id")
	s.add("DELETE", "favorites/destroy/:id", R, jx, s.favoriteDestroy, "id")
	s.add("POST", "favorites/destroy", R, jx, s.favoriteDestroy, "id")

	// Users and account
	s.add("GET", "account/verify_credentials", R, jx, s.verifyCredentials, "")
	s.add("GET", "account/rate_limit_status", O, jx, s.rateLimitStatus, "")
	s.add("", "account/end_session", R, jx, s.endSession, "")
	s.add("POST", "account/update_profile", R, jx, s.verifyCredentials, "name url location description")
	s.add("POST", "account/update_profile_colors", R, jx, s.verifyCredentials, "profile_background_color profile_text_color profile_link_color profile_sidebar_fill_color profile_sidebar_border_color")
	s.add("POST", "account/update_profile_image", R, jx, s.verifyCredentials, "image")
	s.add("POST", "account/update_profile_background_image", R, jx, s.verifyCredentials, "image tile")
	s.add("POST", "account/update_delivery_device", R, jx, s.verifyCredentials, "device")
	s.add("GET", "users/show", O, jx, s.showUser, "id user_id screen_name")
	s.add("GET", "users/show/:id", O, jx, s.showUser, "id")
	s.add("", "users/lookup", R, jx, s.lookupUsers, "user_id screen_name")
	s.add("GET", "users/search", R, jx, s.searchUsers, "q per_page page")
	s.add("GET", "statuses/friends", O, jx, s.friendsList, "id user_id screen_name cursor page")
	s.add("GET", "statuses/friends/:id", O, jx, s.friendsList, "id cursor page")
	s.add("GET", "statuses/followers", R, jx, s.followersList, "id user_id screen_name cursor page")
	s.add("GET", "statuses/followers/:id", O, jx, s.followersList, "id cursor page")

	// Graph
	s.add("POST", "friendships/create", R, jx, s.follow, "id user_id screen_name follow")
	s.add("POST", "friendships/create/:id", R, jx, s.follow, "id follow")
	s.add("POST", "friendships/destroy", R, jx, s.unfollow, "id user_id screen_name")
	s.add("POST", "friendships/destroy/:id", R, jx, s.unfollow, "id")
	s.add("DELETE", "friendships/destroy/:id", R, jx, s.unfollow, "id")
	s.add("GET", "friendships/show", O, jx, s.friendshipShow, "source_id source_screen_name target_id target_screen_name")
	s.add("GET", "friendships/exists", O, jx, s.friendshipExists, "user_a user_b")
	s.add("GET", "friends/ids", O, jx, s.friendIDs, "id user_id screen_name cursor")
	s.add("GET", "friends/ids/:id", O, jx, s.friendIDs, "id cursor")
	s.add("GET", "followers/ids", O, jx, s.followerIDs, "id user_id screen_name cursor")
	s.add("GET", "followers/ids/:id", O, jx, s.followerIDs, "id cursor")
	s.add("POST", "notifications/follow/:id", R, jx, s.showUser, "id")
	s.add("POST", "notifications/leave/:id", R, jx, s.showUser, "id")
	s.add("POST", "notifications/follow", R, jx, s.showUser, "id user_id screen_name")
	s.add("POST", "notifications/leave", R, jx, s.showUser, "id user_id screen_name")
	s.add("POST", "blocks/create/:id", R, jx, s.block, "id")
	s.add("POST", "blocks/create", R, jx, s.block, "id user_id screen_name")
	s.add("POST", "blocks/destroy/:id", R, jx, s.unblock, "id")
	s.add("DELETE", "blocks/destroy/:id", R, jx, s.unblock, "id")
	s.add("POST", "blocks/destroy", R, jx, s.unblock, "id user_id screen_name")
	s.add("GET", "blocks/exists/:id", R, jx, s.blockExists, "id")
	s.add("GET", "blocks/exists", R, jx, s.blockExists, "id user_id screen_name")
	s.add("GET", "blocks/blocking", R, jx, s.blocking, "page")
	s.add("GET", "blocks/blocking/ids", R, jx, s.blockingIDs, "")
	s.add("POST", "report_spam", R, jx, s.block, "id user_id screen_name")
	s.add("POST", "report_spam/:id", R, jx, s.block, "id")

	// Direct messages
	s.add("GET", "direct_messages", R, jx, s.directMessages, "count page since_id max_id")
	s.add("GET", "direct_messages/sent", R, jx, s.directMessagesSent, "count page since_id max_id")
	s.add("POST", "direct_messages/new", R, jx, s.directMessageNew, "user user_id screen_name text")
	s.add("POST", "direct_messages/destroy/:id", R, jx, s.directMessageDestroy, "id")
	s.add("DELETE", "direct_messages/destroy/:id", R, jx, s.directMessageDestroy, "id")
	s.add("POST", "direct_messages/destroy", R, jx, s.directMessageDestroy, "id")

	// Search API (served on every host; search.twitter.com is its home)
	s.add("GET", "search", O, "json atom", s.search, "q rpp page since_id max_id lang locale show_user result_type geocode until since")
	s.add("GET", "trends", O, "json", s.trendsSearch, "exclude")
	s.add("GET", "trends/current", O, "json", s.trendsCurrent, "exclude")
	s.add("GET", "trends/daily", O, "json", s.trendsCurrent, "exclude date")
	s.add("GET", "trends/weekly", O, "json", s.trendsCurrent, "exclude date")
	s.add("GET", "trends/available", N, jx, s.trendsAvailable, "lat long")
	s.add("GET", "trends/:woeid", O, jx, s.trendsLocation, "exclude")

	// Saved searches (stored locally)
	s.add("GET", "saved_searches", R, jx, s.savedSearches, "")
	s.add("GET", "saved_searches/show/:id", R, jx, s.savedSearchShow, "id")
	s.add("POST", "saved_searches/create", R, jx, s.savedSearchCreate, "query")
	s.add("POST", "saved_searches/destroy/:id", R, jx, s.savedSearchDestroy, "id")
	s.add("DELETE", "saved_searches/destroy/:id", R, jx, s.savedSearchDestroy, "id")

	// Misc
	s.add("", "help/test", N, jx, s.helpTest, "")

	// OAuth and xAuth (form-encoded responses)
	s.add("", "oauth/request_token", N, "json", s.oauthRequestToken, "oauth_callback x_auth_access_type")
	s.add("", "oauth/access_token", N, "json", s.oauthAccessToken, "oauth_verifier x_auth_username x_auth_password x_auth_mode")

	// TwitPic-compatible uploads
	s.add("POST", "api/upload", O, "json xml", s.twitpicUpload, "media username password message")
	s.add("POST", "api/uploadAndPost", O, "json xml", s.twitpicUploadAndPost, "media username password message")
	s.add("POST", "2/upload", O, "json xml", s.twitpicUpload, "media message key")
	return s.routes
}
