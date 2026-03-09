package botsky

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/davhofer/indigo/api/atproto"
	"github.com/davhofer/indigo/api/bsky"
	"github.com/davhofer/indigo/atproto/syntax"
	lexutil "github.com/davhofer/indigo/lex/util"
	"github.com/davhofer/indigo/xrpc"
)

const ApiEntryway = "https://bsky.social"
const ApiPublic = "https://public.api.bsky.app"
const ApiChat = "https://api.bsky.chat"

// TODO: need to wrap requests for rate limiting?

// API Client
//
// Wraps an XRPC client for API calls and a second one for handling chat/DMs
type Client struct {
	xrpcClient         *xrpc.Client
	Handle             string
	Did                string
	appkey             string
	refreshProcessLock sync.Mutex   // make sure only one auth refresher runs at a time
	chatClient         *xrpc.Client // client for accessing chat api
	chatCursor         string
}

// Sets up a new client (not yet authenticated)
func NewClient(ctx context.Context, handle string, appkey string) (*Client, error) {
	client := &Client{
		xrpcClient: &xrpc.Client{
			Client: new(http.Client),
			Host:   string(ApiEntryway),
		},
		Handle: handle,
		appkey: appkey,
		chatClient: &xrpc.Client{
			// TODO: reuse the http client?
			Client: new(http.Client),
			Host:   string(ApiChat),
		},
		chatCursor: "",
	}
	// resolve own handle to get did. don't need to be authenticated to do that
	clientDid, err := client.ResolveHandle(ctx, handle)
	if err != nil {
		return nil, err
	}
	client.Did = clientDid
	return client, nil
}

// Resolve the given handle to a DID
//
// If called on a DID, simply returns it
func (c *Client) ResolveHandle(ctx context.Context, handle string) (string, error) {
	if strings.HasPrefix(handle, "did:") {
		return handle, nil
	}
	if strings.HasPrefix(handle, "@") {
		handle = handle[1:]
	}
	output, err := atproto.IdentityResolveHandle(ctx, c.xrpcClient, handle)
	if err != nil {
		return "", fmt.Errorf("ResolveHandle error: %v", err)
	}
	return output.Did, nil
}

// Update the users profile description with the given string. All other profile components (avatar, banner, etc.) stay the same.
func (c *Client) UpdateProfileDescription(ctx context.Context, description string) error {
	profileRecord, err := atproto.RepoGetRecord(ctx, c.xrpcClient, "", "app.bsky.actor.profile", c.Handle, "self")
	if err != nil {
		return fmt.Errorf("UpdateProfileDescription error (RepoGetRecord): %v", err)
	}

	var actorProfile bsky.ActorProfile
	if err := decodeRecordAsLexicon(profileRecord.Value, &actorProfile); err != nil {
		return fmt.Errorf("UpdateProfileDescription error (DecodeRecordAsLexicon): %v", err)
	}

	newProfile := bsky.ActorProfile{
		LexiconTypeID:        "app.bsky.actor.profile",
		Avatar:               actorProfile.Avatar,
		Banner:               actorProfile.Banner,
		CreatedAt:            actorProfile.CreatedAt,
		Description:          &description,
		DisplayName:          actorProfile.DisplayName,
		JoinedViaStarterPack: actorProfile.JoinedViaStarterPack,
		Labels:               actorProfile.Labels,
		PinnedPost:           actorProfile.PinnedPost,
	}

	input := atproto.RepoPutRecord_Input{
		Collection: "app.bsky.actor.profile",
		Record: &lexutil.LexiconTypeDecoder{
			Val: &newProfile,
		},
		Repo:       c.Handle,
		Rkey:       "self",
		SwapRecord: profileRecord.Cid,
	}

	output, err := atproto.RepoPutRecord(ctx, c.xrpcClient, &input)
	if err != nil {
		return fmt.Errorf("UpdateProfileDescription error (RepoPutRecord): %v", err)
	}
	logger.Println("Profile updated:", output.Cid, output.Uri)
	return nil
}

// get posts from bsky API/AppView ***********************************************************

// TODO: method to get post directly from repo?
// Note: this fully relies on bsky api to be built

// Enriched post struct, including both the repo's FeedPost as well as bluesky's PostView
type RichPost struct {
	bsky.FeedPost

	AuthorDid   string // from *bsky.ActorDefs_ProfileViewBasic
	Cid         string
	Uri         string
	IndexedAt   string
	LikeCount   int64
	QuoteCount  int64
	ReplyCount  int64
	RepostCount int64
}

// Load Bluesky AppView postViews for the given repo/user.
//
// Set limit = -1 in order to get all postViews.
func (c *Client) GetPostViews(ctx context.Context, handleOrDid string, limit int) ([]*bsky.FeedDefs_PostView, error) {
	// get all post uris
	postUris, err := c.RepoGetRecordUris(ctx, handleOrDid, "app.bsky.feed.post", limit)
	if err != nil {
		return nil, fmt.Errorf("GetPostViews error (RepoGetRecordUris): %v", err)
	}

	// hydrate'em
	postViews := make([]*bsky.FeedDefs_PostView, 0, len(postUris))
	for i := 0; i < len(postUris); i += 25 {
		j := i + 25
		if j > len(postUris) {
			j = len(postUris)
		}
		results, err := bsky.FeedGetPosts(ctx, c.xrpcClient, postUris[i:j])
		if err != nil {
			return nil, fmt.Errorf("GetPostViews error (FeedGetPosts): %v", err)
		}
		postViews = append(postViews, results.Posts...)
	}
	return postViews, nil
}

// Load enriched posts for repo/user.
//
// Set limit = -1 in order to get all posts.
func (c *Client) GetPosts(ctx context.Context, handleOrDid string, limit int) ([]*RichPost, error) {
	postViews, err := c.GetPostViews(ctx, handleOrDid, limit)
	if err != nil {
		return nil, fmt.Errorf("GetPosts error (GetPostViews): %v", err)
	}

	posts := make([]*RichPost, 0, len(postViews))
	for _, postView := range postViews {
		var feedPost bsky.FeedPost
		if err := decodeRecordAsLexicon(postView.Record, &feedPost); err != nil {
			return nil, fmt.Errorf("GetPosts error (DecodeRecordAsLexicon): %v", err)
		}
		posts = append(posts, &RichPost{
			FeedPost:    feedPost,
			AuthorDid:   postView.Author.Did,
			Cid:         postView.Cid,
			Uri:         postView.Uri,
			IndexedAt:   postView.IndexedAt,
			LikeCount:   *postView.LikeCount,
			QuoteCount:  *postView.QuoteCount,
			ReplyCount:  *postView.ReplyCount,
			RepostCount: *postView.RepostCount,
		})

	}
	return posts, nil
}

// Get a single post by uri.
func (c *Client) GetPost(ctx context.Context, postUri string) (RichPost, error) {
	results, err := bsky.FeedGetPosts(ctx, c.xrpcClient, []string{postUri})
	if err != nil {
		return RichPost{}, fmt.Errorf("GetPost error (FeedGetPosts): %v", err)
	}
	if len(results.Posts) == 0 {
		return RichPost{}, fmt.Errorf("GetPost error: No post with the given uri found")
	}
	postView := results.Posts[0]

	var feedPost bsky.FeedPost
	err = decodeRecordAsLexicon(postView.Record, &feedPost)
	if err != nil {
		return RichPost{}, fmt.Errorf("GetPost error (DecodeRecordAsLexicon): %v", err)
	}

	post := RichPost{
		FeedPost:    feedPost,
		AuthorDid:   postView.Author.Did,
		Cid:         postView.Cid,
		Uri:         postView.Uri,
		IndexedAt:   postView.IndexedAt,
		LikeCount:   *postView.LikeCount,
		QuoteCount:  *postView.QuoteCount,
		ReplyCount:  *postView.ReplyCount,
		RepostCount: *postView.RepostCount,
	}

	return post, nil
}

// Like creates a 'Like' record for a specific post
func (c *Client) Like(ctx context.Context, uri string, cid string) (string, string, error) {
	// Build the record (Following the app.bsky.feed.like Lexicon)
	like := &bsky.FeedLike{
		CreatedAt: time.Now().Format(time.RFC3339),
		Subject: &atproto.RepoStrongRef{
			Uri: uri,
			Cid: cid,
		},
	}

	// Publish it to the "app.bsky.feed.like" collection in Repo
	resp, err := atproto.RepoCreateRecord(ctx, c.xrpcClient, &atproto.RepoCreateRecord_Input{
		Collection: "app.bsky.feed.like",
		Repo:       c.Did,
		Record:     &lexutil.LexiconTypeDecoder{Val: like},
	})

	if err != nil {
		return "", "", err
	}

	return resp.Cid, resp.Uri, nil
}

// Unlike removes a 'Like' record for a specific post.
// likeUri is the URI of the Like record itself (at://did:...)
func (c *Client) Unlike(ctx context.Context, likeUri string) error {
	// 1. Parse the AT-URI string into a structured object
	parsed, err := syntax.ParseATURI(likeUri)
	if err != nil {
		return fmt.Errorf("invalid like URI: %w", err)
	}

	deleteRec := &atproto.RepoDeleteRecord_Input{
		Collection: parsed.Collection().String(), // "app.bsky.feed.like"
		Repo:       c.Did,
		Rkey:       parsed.RecordKey().String(), // The unique ID of that specific Like
	}

	_, err = atproto.RepoDeleteRecord(ctx, c.xrpcClient, deleteRec)
	if err != nil {
		return err
	}

	return nil
}

// Follow follows a user by their DID.
func (c *Client) Follow(ctx context.Context, targetDid string) (string, string, error) {
	// Build the Record (Following the app.bsky.graph.follow Lexicon)
	follow := &bsky.GraphFollow{
		CreatedAt: time.Now().Format(time.RFC3339),
		Subject:   targetDid,
	}

	// Publish it to the "app.bsky.graph.follow" collection in Repo
	resp, err := atproto.RepoCreateRecord(ctx, c.xrpcClient, &atproto.RepoCreateRecord_Input{
		Collection: "app.bsky.graph.follow",
		Repo:       c.Did,
		Record:     &lexutil.LexiconTypeDecoder{Val: follow},
	})

	if err != nil {
		return "", "", err
	}

	return resp.Cid, resp.Uri, nil
}

// Unfollow removes a 'Follow' record.
// followUri is the URI of the Follow record itself (e.g., at://did:...)
func (c *Client) Unfollow(ctx context.Context, followUri string) error {

	// extract the collection and rkey

	parsed, err := syntax.ParseATURI(followUri)
	if err != nil {
		return fmt.Errorf("invalid follow URI: %w", err)
	}

	deleteRec := &atproto.RepoDeleteRecord_Input{
		Collection: parsed.Collection().String(), // app.bsky.graph.follow
		Repo:       c.Did,
		Rkey:       parsed.RecordKey().String(), // The unique ID of that follow
	}

	_, err = atproto.RepoDeleteRecord(ctx, c.xrpcClient, deleteRec)
	if err != nil {
		return err
	}

	return nil
}

type TimelinePost struct {
	Uri         string
	Cid         string
	Author      string
	AuthorDid   string
	Text        string
	ReplyCount  int64
	QuoteCount  int64
	RepostCount int64
	LikeCount   int64
	IndexedAt   string
}

// GetTimeline fetches the authenticated user's home timeline.
// limit: how many posts (max 100). cursor: for pagination (optional).
func (c *Client) GetTimeline(ctx context.Context, limit int64) ([]TimelinePost, error) {
	// 1. Raw Indigo Call
	out, err := bsky.FeedGetTimeline(ctx, c.xrpcClient, "", "", limit)
	if err != nil {
		return nil, err
	}

	var posts []TimelinePost
	for _, item := range out.Feed {
		// Type assertion: Is this actually a post record?
		postRec, ok := item.Post.Record.Val.(*bsky.FeedPost)
		if !ok {
			continue
		}

		posts = append(posts, TimelinePost{
			Uri:         item.Post.Uri,
			Cid:         item.Post.Cid,
			Author:      item.Post.Author.Handle,
			AuthorDid:   item.Post.Author.Did,
			Text:        postRec.Text,
			ReplyCount:  *item.Post.ReplyCount,
			LikeCount:   *item.Post.LikeCount,
			RepostCount: *item.Post.RepostCount,
			IndexedAt:   item.Post.IndexedAt,
		})
	}
	return posts, nil
}
