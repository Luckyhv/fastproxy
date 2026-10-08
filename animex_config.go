package main

import "net/url"

// Animex provider settings. Edit these here and rebuild when the provider changes.
// Nero currently uses REST; the GraphQL URL is kept with the other settings.
const (
	animexGraphQLURL = "https://graphql.animex.one/graphql"
	animexRESTURL    = "https://pp.animex.one/rest/api"
	animexOrigin     = "https://animex.one"
	animexAPIKey     = "a3f9c1d8e7b2c4d6f1a9b0c3d4e5f6789a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d"
)

// Parse once so routing follows the configured REST host without per-request work.
var animexRESTEndpoint = func() *url.URL {
	endpoint, err := url.Parse(animexRESTURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		panic("invalid Animex REST URL")
	}
	return endpoint
}()
