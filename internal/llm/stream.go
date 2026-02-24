package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ChunkCallback is invoked periodically during streaming with the accumulated text so far.
type ChunkCallback func(textSoFar string)

// StreamCollect calls Stream and collects chunks into a single Message.
func StreamCollect(ctx context.Context, m model.BaseChatModel, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return StreamCollectWithCallback(ctx, m, input, nil, opts...)
}

// StreamCollectWithCallback is like StreamCollect but invokes onChunk (throttled
// to 200ms) with accumulated text. A final callback fires after the stream completes.
func StreamCollectWithCallback(ctx context.Context, m model.BaseChatModel, input []*schema.Message, onChunk ChunkCallback, opts ...model.Option) (*schema.Message, error) {
	reader, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var chunks []*schema.Message
	var accum strings.Builder
	var lastCallback time.Time

	for {
		chunk, err := reader.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream recv: %w", err)
		}
		chunks = append(chunks, chunk)

		if onChunk != nil {
			accum.WriteString(chunk.Content)
			if time.Since(lastCallback) >= 200*time.Millisecond {
				onChunk(accum.String())
				lastCallback = time.Now()
			}
		}
	}

	if len(chunks) == 0 {
		return nil, fmt.Errorf("empty stream response")
	}

	if onChunk != nil {
		onChunk(accum.String())
	}

	return schema.ConcatMessages(chunks)
}
