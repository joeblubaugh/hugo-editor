# hugo-editor is a frontend for a Hugo blog

It provides a simple textarea-based editor for posts, which periodically saves to markdown files located in `site/content/blog/`. I wrote it to make it easier to write and publish posts for [my own blog](https://joeblu.com). It doesn't support any directory hierarchies beneath `blog/`. It doesn't support image uploading or browsing. There is no authentication. I recommend exposing the endpoints only on a trusted network like a private Tailscale network, or using your web-server to provide basic authentication support. [Here's how to do that in Caddy](https://caddyserver.com/docs/caddyfile/directives/basic_auth).

You can provide a custom publish command. When clicking "publish" on a post's page, hugo-editor will make a git commit with the post that is being edited, push to the remote, and run the custom publish command. In my case, the publish command is an `rsync` to my site's server.

It's extremely simple, and intended to be so. The project is designed to provide remote editor for your blog posts, to be a CMS for Hugo. It should be easy to fork and modify if you want different directory hierarchies, or a different starting template for your blog post.

## Development

The server is a Go binary that has zero dependencies outside of the standard library. It uses `html/template` to serve statically generated pages, with very light javascript to support auto-save.

Just `go run cmd/hugo-editor/main.go --site $PATH_TO_HUGO_SITE_DIRECTORY` when developing - it's easy to configure [Reflex](https://github.com/cespare/reflex) or [Air](https://github.com/air-verse/air) if you like.
