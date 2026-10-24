package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ownerOf reports the task that owns a chat_message, if any.
func ownerOf(t *testing.T, pool *pgxpool.Pool, messageID string) pgtype.UUID {
	t.Helper()
	var owner pgtype.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT task_id FROM chat_message WHERE id = $1`, messageID).Scan(&owner); err != nil {
		t.Fatalf("load message owner: %v", err)
	}
	return owner
}

// TestSealChannelInputIgnoresTurnlessAssistantRows is MP-69's regression: a
// server-authored mirror landing between the user message and the debounce
// flush must not be read as "already answered".
//
// The mirror is an assistant row with task_id NULL — no producing turn — which
// by definition answered no batch; MP-68 stamps exactly these rows for the NEXT
// turn to read. Treating one as a boundary hid the waiting user message from
// that turn, sealed zero rows, and the claim cancelled the task for having an
// empty input batch (GH #6591).
//
// The same run also pins acceptance 3: channel_command rows stay excluded from
// the batch whether or not a mirror sits next to them.
func TestSealChannelInputIgnoresTurnlessAssistantRows(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	fixture := seedChatContextFixture(t, pool)

	commandAt := time.Now().Add(-3 * time.Minute)
	command := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "user", kind: "channel_command", content: "/issue 已经同步处理过",
		createdAt: commandAt,
	})
	messageAt := commandAt.Add(10 * time.Second)
	waiting := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "user", content: "刚才那条评审结果还差什么？", createdAt: messageAt,
	})
	// The mirror: server-authored review result, no producing turn, newer than
	// the message it must not hide.
	mirror := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindReviewResult,
		content:   protocol.BuildReviewResultContent(true, "阻塞项已修复。", "MP-68"),
		createdAt: messageAt.Add(500 * time.Millisecond),
	})

	taskID := insertChatTask(t, pool, fixture, messageAt.Add(time.Second))
	if err := q.LinkUnownedChannelChatMessagesToTask(ctx, db.LinkUnownedChannelChatMessagesToTaskParams{
		TaskID: util.MustParseUUID(taskID), ChatSessionID: util.MustParseUUID(fixture.sessionID),
	}); err != nil {
		t.Fatalf("seal channel input: %v", err)
	}

	if got := util.UUIDToString(ownerOf(t, pool, waiting)); got != taskID {
		t.Fatalf("waiting message owner = %q, want the new turn %s: the mirror hid it from its own turn", got, taskID)
	}
	for _, want := range []struct {
		name string
		id   string
	}{{"channel_command row", command}, {"mirror", mirror}} {
		if got := ownerOf(t, pool, want.id); got.Valid {
			t.Errorf("%s was sealed into the batch as %s", want.name, util.UUIDToString(got))
		}
	}
}

// TestSealChannelInputKeepsRealReplyAsBoundary is acceptance 2: a reply that
// HAS a producing turn keeps the exact boundary semantics it had before MP-69.
//
// Both directions are already load-bearing. A reply from a turn that started
// after the message is a genuine answer, so the message stays unowned. A reply
// from a turn that was already running when the message landed had sealed its
// input before the message existed, so it answers an earlier batch and must not
// block the new one (GH #6591).
func TestSealChannelInputKeepsRealReplyAsBoundary(t *testing.T) {
	cases := []struct {
		name        string
		replyTurnAt func(messageAt time.Time) time.Time
		wantSealed  bool
		why         string
	}{
		{
			name:        "turn started after the message answered it",
			replyTurnAt: func(messageAt time.Time) time.Time { return messageAt.Add(30 * time.Second) },
			wantSealed:  false,
			why:         "a later turn already answered this message; sealing it would re-answer it in the new batch",
		},
		{
			name:        "turn was already running when the message landed",
			replyTurnAt: func(messageAt time.Time) time.Time { return messageAt.Add(-time.Minute) },
			wantSealed:  true,
			why:         "the reply belongs to an earlier batch and must not hide this message (GH #6591)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := sharedTestPool(t)
			ctx := context.Background()
			q := db.New(pool)
			fixture := seedChatContextFixture(t, pool)

			messageAt := time.Now().Add(-10 * time.Minute)
			waiting := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
				role: "user", content: "帮我看下这个", createdAt: messageAt,
			})
			replyTurn := insertChatTask(t, pool, fixture, tc.replyTurnAt(messageAt))
			insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
				role: "assistant", content: "好的。", taskID: replyTurn,
				createdAt: messageAt.Add(time.Second),
			})

			taskID := insertChatTask(t, pool, fixture, messageAt.Add(2*time.Second))
			if err := q.LinkUnownedChannelChatMessagesToTask(ctx, db.LinkUnownedChannelChatMessagesToTaskParams{
				TaskID: util.MustParseUUID(taskID), ChatSessionID: util.MustParseUUID(fixture.sessionID),
			}); err != nil {
				t.Fatalf("seal channel input: %v", err)
			}

			got := util.UUIDToString(ownerOf(t, pool, waiting))
			if tc.wantSealed && got != taskID {
				t.Fatalf("message owner = %q, want the new turn %s: %s", got, taskID, tc.why)
			}
			if !tc.wantSealed && got == taskID {
				t.Fatalf("message was sealed into %s: %s", taskID, tc.why)
			}
		})
	}
}

// TestSealChannelInputKeepsReplyWhoseTaskRowIsGoneAsBoundary is the other half
// of acceptance 2: MP-69 only stops trusting rows that never had a producing
// turn. A row that HAS a task_id but whose task row is gone is unattributable
// output, still treated as possibly answering — the batch stays empty.
func TestSealChannelInputKeepsReplyWhoseTaskRowIsGoneAsBoundary(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	fixture := seedChatContextFixture(t, pool)

	messageAt := time.Now().Add(-10 * time.Minute)
	waiting := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "user", content: "这条会不会被重新回答", createdAt: messageAt,
	})
	// A task id with no row: chat_message.task_id has no foreign key, so this is
	// exactly what a legacy row whose owning turn was reaped looks like.
	const vanishedTask = "00000000-0000-7000-8000-00000000dead"
	insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", content: "无法归属的旧输出", taskID: vanishedTask,
		createdAt: messageAt.Add(time.Second),
	})

	taskID := insertChatTask(t, pool, fixture, messageAt.Add(2*time.Second))
	if err := q.LinkUnownedChannelChatMessagesToTask(ctx, db.LinkUnownedChannelChatMessagesToTaskParams{
		TaskID: util.MustParseUUID(taskID), ChatSessionID: util.MustParseUUID(fixture.sessionID),
	}); err != nil {
		t.Fatalf("seal channel input: %v", err)
	}

	if got := util.UUIDToString(ownerOf(t, pool, waiting)); got == taskID {
		t.Fatalf("message was sealed into %s behind unattributable output", taskID)
	}
}
