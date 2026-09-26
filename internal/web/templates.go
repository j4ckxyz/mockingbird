package web

import "html/template"

// Pages must render in iPhone OS 3 Mobile Safari (WebKit 528, 2009): no
// JavaScript, no CSS custom properties, prefixed border-radius, and a tiny
// inline stylesheet. Modern browsers additionally honour
// prefers-color-scheme; a cookie-backed toggle overrides it without JS.
const css = `
body{margin:0;padding:0;background:#c0deed;color:#333;font:14px/1.4 "Lucida Grande","Helvetica Neue",Helvetica,Arial,sans-serif;-webkit-text-size-adjust:none}
a{color:#0084b4;text-decoration:none}a:hover{text-decoration:underline}
.wrap{max-width:640px;margin:0 auto;padding:12px}
.hd{padding:8px 4px 12px}
.logo{font:bold 26px/1 Georgia,"Times New Roman",serif;color:#fff;text-shadow:0 1px 0 #7fb2cc;letter-spacing:-1px}
.logo a{color:#fff}.tag{color:#fff;font-size:13px;margin-top:4px}
.box{background:#fff;border:1px solid #a9cfe3;-webkit-border-radius:8px;border-radius:8px;padding:14px 16px;margin:0 0 12px}
h1{font-size:20px;margin:0 0 8px;color:#333}h2{font-size:16px;margin:16px 0 6px;color:#333}
p{margin:0 0 10px}ol,ul{margin:0 0 10px;padding-left:22px}li{margin-bottom:4px}
code{font:12px Monaco,Menlo,"Courier New",monospace;background:#f2f7fa;padding:1px 4px;-webkit-border-radius:3px;border-radius:3px;word-wrap:break-word}
.warn{background:#fff6d6;border:1px solid #e8c84a;-webkit-border-radius:6px;border-radius:6px;padding:10px 12px;margin:0 0 12px;color:#5a4a00}
.err{background:#fde8e8;border:1px solid #e39a9a;-webkit-border-radius:6px;border-radius:6px;padding:10px 12px;margin:0 0 12px;color:#7a1f1f}
label{display:block;font-weight:bold;margin:10px 0 4px}
input.t{width:95%;font-size:16px;padding:6px;border:1px solid #aaa;-webkit-border-radius:4px;border-radius:4px}
.btn{margin-top:14px;font-size:16px;font-weight:bold;color:#fff;background:#2a8ad0;border:1px solid #1f6fa8;padding:7px 18px;-webkit-border-radius:5px;border-radius:5px}
.pin{font:bold 32px Monaco,Menlo,monospace;letter-spacing:4px;text-align:center;margin:12px 0}
.post .who{overflow:hidden;margin-bottom:8px}.post .av{float:left;width:48px;height:48px;margin-right:10px;-webkit-border-radius:4px;border-radius:4px}
.post .nm{font-weight:bold;color:#333}.post .h{color:#999}.post .txt{font-size:18px;line-height:1.35;margin:4px 0 10px;word-wrap:break-word}
.post .img{display:block;max-width:100%;margin:8px 0;-webkit-border-radius:4px;border-radius:4px}
.post .meta{color:#999;font-size:12px}
.quote{border:1px solid #ddd;-webkit-border-radius:6px;border-radius:6px;padding:8px 10px;margin:8px 0}
.card{border:1px solid #ddd;-webkit-border-radius:6px;border-radius:6px;padding:8px 10px;margin:8px 0;color:#333}
.ft{text-align:center;color:#5a8aa3;font-size:12px;padding:6px 0 18px}.ft a{color:#fff}
.dim{color:#999}
html.dark body{background:#15202b;color:#d9e1e8}
html.dark .box{background:#192734;border-color:#2c3e50}html.dark h1,html.dark h2,html.dark .post .nm,html.dark .card{color:#e8eef3}
html.dark code{background:#22303c}html.dark a{color:#4ab3f4}html.dark .logo{text-shadow:none}
html.dark .quote,html.dark .card{border-color:#38444d}html.dark .warn{background:#3a3212;border-color:#7a6620;color:#f2e2a0}
html.dark .err{background:#3d1c1c;border-color:#7a3434;color:#f5c2c2}html.dark .ft{color:#8899a6}html.dark .ft a{color:#4ab3f4}
html.dark input.t{background:#22303c;color:#fff;border-color:#38444d}
@media (prefers-color-scheme:dark){
html.auto body{background:#15202b;color:#d9e1e8}
html.auto .box{background:#192734;border-color:#2c3e50}html.auto h1,html.auto h2,html.auto .post .nm,html.auto .card{color:#e8eef3}
html.auto code{background:#22303c}html.auto a{color:#4ab3f4}html.auto .logo{text-shadow:none}
html.auto .quote,html.auto .card{border-color:#38444d}html.auto .warn{background:#3a3212;border-color:#7a6620;color:#f2e2a0}
html.auto .err{background:#3d1c1c;border-color:#7a3434;color:#f5c2c2}html.auto .ft{color:#8899a6}html.auto .ft a{color:#4ab3f4}
html.auto input.t{background:#22303c;color:#fff;border-color:#38444d}
}
`

const layout = `{{define "top"}}<!DOCTYPE html>
<html lang="en" class="{{.Theme}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<link rel="icon" href="/static/favicon.ico">
<style>` + css + `</style></head><body><div class="wrap">
<div class="hd"><div class="logo"><a href="/">mockingbird</a></div><div class="tag">Your 2009 Twitter client, now on Bluesky.</div></div>
{{end}}
{{define "bottom"}}<div class="ft">Theme:
{{if eq .Theme "auto"}}<b>auto</b>{{else}}<a href="/theme?set=auto&amp;r={{.Here}}">auto</a>{{end}} &middot;
{{if eq .Theme "light"}}<b>light</b>{{else}}<a href="/theme?set=light&amp;r={{.Here}}">light</a>{{end}} &middot;
{{if eq .Theme "dark"}}<b>dark</b>{{else}}<a href="/theme?set=dark&amp;r={{.Here}}">dark</a>{{end}}
<br>mockingbird is not affiliated with Twitter or Bluesky.</div></div></body></html>{{end}}`

var tmpl = template.Must(template.New("pages").Parse(layout + `
{{define "home"}}{{template "top" .}}
<div class="warn"><b>Read this first.</b> Vintage clients talk to this bridge over plain HTTP (and at best TLS 1.0), so your handle and password can be seen by anyone on the same network. That is why mockingbird <b>only accepts Bluesky app passwords</b> and refuses your main password. An app password can be revoked at any time without touching your account. Use a fresh one just for this, and revoke it if you stop using the bridge.</div>

<div class="box"><h1>Set up in two minutes</h1>
<ol>
<li>On a modern device, open <a href="https://bsky.app/settings/app-passwords">bsky.app &rsaquo; Settings &rsaquo; Privacy and security &rsaquo; App passwords</a> and create one. Tick <i>Allow access to your direct messages</i> if you want DMs to work.</li>
<li>In your client, use your Bluesky handle as the username (for example <code>alice.bsky.social</code>; just <code>alice</code> works for <code>.{{.DefaultHost}}</code> accounts) and the app password as the password.</li>
<li>Point the client at the bridge, using the instructions for your client below.</li>
</ol></div>

<div class="box"><h2>Any client with a custom API root</h2>
<p>REST API root: <code>{{.Base}}/</code> (or <code>{{.Base}}/1/</code>)<br>
Search API root: <code>{{.Base}}/</code> (search lives at <code>{{.Base}}/search.json</code>)<br>
OAuth endpoints: <code>{{.Base}}/oauth/request_token</code>, <code>/oauth/authorize</code>, <code>/oauth/access_token</code><br>
Image service (TwitPic-compatible): <code>{{.Base}}/api/upload</code></p></div>

<div class="box"><h2>Tweetie 2 and Twitterrific 1.x (hardcoded hosts)</h2>
<p>These clients always call <code>twitter.com</code>, <code>api.twitter.com</code> and <code>search.twitter.com</code>, and have no setting to change that. They need those names to point at a bridge on your own network.</p>
{{if .IP}}<ul>
<li><b>Simulator:</b> add a line like <code>{{.IP}} twitter.com api.twitter.com search.twitter.com</code> to <code>/etc/hosts</code> on the Mac running it.</li>
<li><b>Real device:</b> use a DNS server you control (Pi-hole, dnsmasq or your router) to answer those three names with this bridge's address, and set it as the device's DNS server under Settings &rsaquo; Wi-Fi.</li>
</ul>
<p>Tweetie 2 signs in with Basic Auth or xAuth; Twitterrific 1.x uses Basic Auth and XML. Both work over plain HTTP.</p>
{{else}}<p>This hosted bridge sits behind a content network, so redirecting <code>twitter.com</code> to it won't work. To use these apps, run your own copy of mockingbird on your home network (it's one small Docker container) and point those names at it with your router or Pi-hole. Apps with a custom API root setting can use this hosted bridge directly.</p>{{end}}
</div>

{{if .TLS}}<div class="box"><h2>HTTPS for clients that insist on it</h2>
<p>Some clients only use <code>https://</code>. The bridge runs a legacy TLS 1.0 listener with a certificate from its own certificate authority. To trust it, open this page in Mobile Safari on the device and install:</p>
<p><a href="/mockingbird.mobileconfig">Install profile (.mobileconfig)</a> &middot; <a href="/ca.crt">Download CA certificate</a></p>
<div class="warn">Installing this CA means your device trusts certificates the bridge issues{{if .NameConstrained}} (limited to {{.TLSHosts}}){{end}}. Anyone who obtained the bridge's CA key could impersonate those sites to your device. Only install it on a device you use for vintage apps, and remove it from Settings &rsaquo; General &rsaquo; Profiles when you are done.</div>
<p class="dim">SHA-256 fingerprint: <code>{{.CAFingerprint}}</code></p></div>{{end}}

<div class="box"><h2>What works</h2>
<p>Home, mentions and user timelines, posting and replying, reposts (as retweets), likes (as favourites), following, blocking, search, trends, direct messages (with a DM-enabled app password), and image uploads. Images and quote posts appear as links to lightweight pages that old browsers can open. Posts can be up to 300 characters, although your client may still count down from 140.</p></div>
{{template "bottom" .}}{{end}}

{{define "authorize"}}{{template "top" .}}
<div class="box"><h1>Sign in to use this app</h1>
{{if .Page.Error}}<div class="err">{{.Page.Error}}</div>{{end}}
{{if .Page.PIN}}<p>You're signed in. Enter this PIN in your app to finish:</p><div class="pin">{{.Page.PIN}}</div>
{{else if .Page.Token}}<p>An app wants to use your Bluesky account through mockingbird. Use an <b>app password</b>, never your main password.</p>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="oauth_token" value="{{.Page.Token}}">
<label for="handle">Bluesky handle</label><input class="t" id="handle" name="handle" type="text" autocapitalize="off" autocorrect="off" value="{{.Page.Handle}}" placeholder="alice.bsky.social">
<label for="password">App password</label><input class="t" id="password" name="password" type="password" placeholder="xxxx-xxxx-xxxx-xxxx">
<br><input class="btn" type="submit" value="Allow">
</form>
<p class="dim" style="margin-top:12px">Credentials may cross the network unencrypted. That is why only revocable app passwords are accepted.</p>{{end}}
</div>
{{template "bottom" .}}{{end}}

{{define "post"}}{{template "top" .}}
<div class="box post">
<div class="who"><img class="av" src="{{.Post.Avatar}}" width="48" height="48" alt="">
<div><span class="nm">{{.Post.Name}}</span><br><span class="h">@{{.Post.Handle}}</span></div></div>
<div class="txt">{{range .Post.Segments}}{{if .Href}}<a href="{{.Href}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{end}}</div>
{{range .Post.Images}}<a href="{{.Full}}"><img class="img" src="{{.Thumb}}" alt="{{.Alt}}"></a>{{if .Alt}}<div class="dim">{{.Alt}}</div>{{end}}{{end}}
{{if .Post.Video}}<p class="dim">This post has a video. <a href="{{.Post.BskyURL}}">Watch it on Bluesky</a>.</p>{{end}}
{{with .Post.Card}}<a class="card" href="{{.URI}}" style="display:block"><b>{{.Title}}</b><br><span class="dim">{{.URI}}</span></a>{{end}}
{{with .Post.Quote}}<div class="quote"><b>{{.Name}}</b> <span class="dim">@{{.Handle}}</span><br>{{.Text}}{{if .Page}}<br><a href="{{.Page}}">Open</a>{{end}}</div>{{end}}
<div class="meta">{{.Post.When}} &middot; <a href="{{.Post.BskyURL}}">View on bsky.app</a></div>
</div>
{{template "bottom" .}}{{end}}

{{define "message"}}{{template "top" .}}<div class="box"><h1>{{.Heading}}</h1><p>{{.Message}}</p></div>{{template "bottom" .}}{{end}}
`))
