# httpcache

`httpcache` is an extremely simple Go HTTP round-tripper that adds file-based caching for custom URL schemes.

## Schemes

- `cache://`:
1. Hashes the request URL with SHA-256 and uses it as the cache file name.
2. If cached content exists, sends `If-Modified-Since` based on cache file mtime.
3. If upstream returns a fresh successful response, updates cache and returns it.
4. If upstream returns `304 Not Modified` (or cannot provide a usable response), returns cached content.

- `cachez://`:
1. Returns cached content only.
2. Does not contact upstream.
3. Returns an error on cache miss.

## Installation

```bash
go get github.com/tired-engineer/httpcache
```

## Usage

```go
package main

import (
	"fmt"
	"io"
	"net/http"

	"github.com/tired-engineer/httpcache"
)

func main() {
	transport := &http.Transport{}

	if err := httpcache.AddCacheRoundTripper("./.httpcache", nil, transport); err != nil {
		panic(err)
	}

	client := &http.Client{Transport: transport}

	resp, err := client.Get("cache://example.com/")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}
```

You can also chain it directly:

```go
base := http.DefaultTransport
cached, err := httpcache.NewRoundTripper("./.httpcache", base)
if err != nil {
	panic(err)
}

client := &http.Client{Transport: cached}
```

## License

MIT
