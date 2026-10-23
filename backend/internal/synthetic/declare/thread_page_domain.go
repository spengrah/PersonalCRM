package declare

import (
	"fmt"
	"strings"
)

// Thread-page resolutions (IXN-078 to IXN-086, IXN-097): a thread's page, with Review
// off and on. A browser test opens a thread of a declared world, records the
// run's judgments of its threads through the judgment store's testing-only
// route, naming each thread's messages by the keys the thread read returns,
// then drives the page.
func init() {
	RegisterNone("IXN-078", "its citing tests seed the IXN-076 world, whose Telegram chats of one contact fill two pages of the list, and open its threads' pages from the list's addresses")
	RegisterNone("IXN-079", "its citing tests seed the IXN-080 world and open the thread of its long chat with Review off")
	Register(Declaration{Behavior: "IXN-080", Entities: reviewWorld()})
	RegisterNone("IXN-081", "its citing tests seed the IXN-080 world and record runs of the compared methods through the testing route, flagging messages of its chats, then label the chips")
	RegisterNone("IXN-082", "its citing tests seed the IXN-080 world and record a run whose later nights hold clearing decisions of earlier commitment flags through the testing route")
	RegisterNone("IXN-083", "its citing tests seed the IXN-080 world and record runs through the testing route, label chips, then close the threads by Done and by Next")
	RegisterNone("IXN-084", "its citing tests seed the IXN-080 world and record nights that flag nothing through the testing route, then confirm their Nones")
	RegisterNone("IXN-085", "its citing tests seed the IXN-080 world and record a run through the testing route, then hold the review saves' responses in the browser")
	RegisterNone("IXN-086", "its citing tests seed the IXN-080 world and record a run whose judgments answer ball-in-court questions through the testing route, then review the reply lists")
	Register(Declaration{Behavior: "IXN-097", Entities: quotedHistoryWorld()})
}

// quotedHistoryWorld is three contacts, each in one email thread of one
// message. Ana's message starts its thread carrying a short quote and Ben's a
// quote longer than ingest keeps, so its quoted history ends trimmed; Cai's
// message carries none.
func quotedHistoryWorld() []Entity {
	long := []string{"On Mon, Synthetic Sender wrote:"}
	for i := range 150 {
		long = append(long, fmt.Sprintf("> Earlier synthetic line %03d of a long quoted conversation.%s", i, strings.Repeat(" filler", 4)))
	}
	return []Entity{
		Contact("ana", Methods(MethodEmail)),
		Contact("ben", Methods(MethodEmail)),
		Contact("cai", Methods(MethodEmail)),
		MessageInteraction("short-quote", "ana", "email", AgoDays(3),
			StartsThreadQuoting("On Mon, Synthetic Sender wrote:\n> Earlier synthetic question?")),
		MessageInteraction("long-quote", "ben", "email", AgoDays(3), StartsThreadQuoting(strings.Join(long, "\n"))),
		MessageInteraction("no-quote", "cai", "email", AgoDays(3)),
	}
}

// longChatMessages is the length of the review world's long chat: more messages
// than a thread page first shows with Review off, spread over three days.
const longChatMessages = 30

// reviewWorld is three contacts in three Telegram group chats. The board chat
// holds longChatMessages messages the three speak in turn, ten a day over three
// days, so a night's window can span two days. The side chat holds three
// messages of two of them, and the quiet chat two messages of the third, who
// is in no other chat, so a run can leave a night with nothing to show.
func reviewWorld() []Entity {
	members := []string{"ana", "ben", "cai"}
	var board []GroupMessage
	for i := range longChatMessages {
		day, slot := i/10, i%10
		board = append(board, Said(members[i%len(members)], day*24*60+slot*7))
	}
	return []Entity{
		Contact("ana", Methods(MethodEmail, MethodTelegram)),
		Contact("ben", Methods(MethodEmail, MethodTelegram)),
		Contact("cai", Methods(MethodEmail, MethodTelegram)),
		TelegramGroupChat("board", members, AgoDays(6), board...),
		TelegramGroupChat("side", []string{"ana", "ben"}, AgoDays(3), Said("ben", 0), Said("ana", 2), Said("ben", 5)),
		TelegramGroupChat("quiet", []string{"cai"}, AgoDays(2), Said("cai", 0), Said("cai", 3)),
	}
}
