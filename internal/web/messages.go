package web

// Msg is a key for a message shown at the top of the page. Only these fixed
// texts can be shown, so nothing from a request is ever echoed back.
type Msg string

func (m Msg) Error() string { return string(m) }

// Message keys.
const (
	MsgSaved          Msg = "saved"
	MsgTrusted        Msg = "trusted"
	MsgRemovedLocal   Msg = "removed_local"
	MsgRemoved        Msg = "removed"
	MsgTimedOut       Msg = "timed_out"
	MsgRejected       Msg = "rejected"
	MsgMismatch       Msg = "mismatch"
	MsgBadCode        Msg = "bad_code"
	MsgCodeFormat     Msg = "code_format"
	MsgLinksOff       Msg = "links_off"
	MsgThrottled      Msg = "throttled"
	MsgCantReach      Msg = "cant_reach"
	MsgCancelled      Msg = "cancelled"
	MsgAccessFormat   Msg = "access_format"
	MsgNotReady       Msg = "not_ready"
	MsgUnknownPrinter Msg = "unknown_printer"
	MsgNothingToTrust Msg = "nothing_to_trust"
	MsgSaveFailed     Msg = "save_failed"
	MsgError          Msg = "error"
	MsgBadForm        Msg = "bad_form"
	MsgAlreadyPaired  Msg = "already_paired"
	MsgPrinterRemoved Msg = "printer_removed"
	MsgUnknownLink    Msg = "unknown_link"
	// 0.3: phone alerts through Home Assistant.
	MsgHASaved   Msg = "ha_saved"
	MsgHARemoved Msg = "ha_removed"
	MsgHAFormat  Msg = "ha_format"
	MsgHATested  Msg = "ha_tested"
	MsgHAFailed  Msg = "ha_failed"
)

// messages is customer facing copy. Plain short sentences, no dashes.
var messages = map[Msg]string{
	MsgSaved:          "Saved. Checking the printer...",
	MsgTrusted:        "Trusted. Checking the printer...",
	MsgRemovedLocal:   "Removed. To connect again, get a new code in Spoken Parts.",
	MsgRemoved:        "This link was removed in Spoken Parts. Printer access codes were deleted. To connect again, get a new code.",
	MsgTimedOut:       "This code timed out. Get a new code in Spoken Parts.",
	MsgRejected:       "This link was not approved. To try again, get a new code in Spoken Parts.",
	MsgMismatch:       "The check number from Spoken Parts did not match this link, so we stopped. Get a new code in Spoken Parts.",
	MsgBadCode:        "That code didn't work. Check it, or get a new code in Spoken Parts.",
	MsgCodeFormat:     "The code has 8 letters and numbers. Check it and try again.",
	MsgLinksOff:       "Spoken Parts isn't taking new links right now. Try again later.",
	MsgThrottled:      "Too many tries. Wait a few minutes, then try again.",
	MsgCantReach:      "Can't reach Spoken Parts. Check that Home Assistant has internet, then try again.",
	MsgCancelled:      "Stopped. You can enter a new code anytime.",
	MsgAccessFormat:   "An access code has exactly 8 letters or numbers. Check it on the printer.",
	MsgNotReady:       "Connect to Spoken Parts first. Then you can type access codes.",
	MsgUnknownPrinter: "We don't see that printer anymore. Refresh the page.",
	MsgNothingToTrust: "There is nothing to trust for this printer right now. Refresh the page.",
	MsgSaveFailed:     "We couldn't save that. Try again.",
	MsgError:          "Something went wrong. Try again.",
	MsgBadForm:        "This page was open too long. Try again.",
	MsgAlreadyPaired:  "This link is already connected.",
	MsgPrinterRemoved: "Removed this printer here. It shows up again when it is on. Then type its access code again.",
	MsgUnknownLink:    "This link isn't known to Spoken Parts any more. Get a new code and pair again.",
	MsgHASaved:        "Saved. Problems now also go to your phone through Home Assistant.",
	MsgHARemoved:      "Removed. Problems no longer go to your phone through Home Assistant.",
	MsgHAFormat:       "The webhook ID is 24 to 128 letters, numbers, dashes or underscores. Copy it from the automation.",
	MsgHATested:       "Test sent. Check your phone.",
	MsgHAFailed:       "Home Assistant did not take the test. Check that the automation is on and the webhook ID matches.",
}

// goodMessages are shown in the calm style; the rest as a warning.
var goodMessages = map[Msg]bool{
	MsgSaved: true, MsgTrusted: true, MsgRemovedLocal: true, MsgCancelled: true, MsgPrinterRemoved: true,
	MsgHASaved: true, MsgHARemoved: true, MsgHATested: true,
}

// Text returns the copy for m, or "".
func (m Msg) Text() string { return messages[m] }

// Good reports whether m is a success message.
func (m Msg) Good() bool { return goodMessages[m] }
