package adsetup

import "strings"

// manualSteps are the numbered steps for a check MarketFlow cannot fix itself, by item key (the part
// before ":"). Provider menus as of 2026-11-25. An item's own steps win.
var manualSteps = map[string][]string{
	"google.account": {
		"Open Google Ads and pick this account at the top right.",
		"Go to Billing, then Payment methods, and add a card or another way to pay.",
		"Follow any red or yellow notice at the top of the page.",
		"Come back here and press Check again.",
	},
	"google.permission": {
		"Press Reconnect Google Ads.",
		"On the Google Ads card press Add or change accounts.",
		"On Google's screen leave every permission ticked and press Continue.",
		"Choose the same account, then come back here and press Check again.",
	},
	"google.manager": {
		"Open Google Ads and pick this account at the top right.",
		"Go to Admin, Access and security, then the Managers tab.",
		"Accept MarketFlow's request if one is waiting. If there is none, ask MarketFlow support to send it.",
	},
	"google.conversion_action": {
		"Open Goals, then Conversions, Summary in Google Ads.",
		"Open the conversion named here and press Edit settings.",
		"Under Goal and action optimization choose Primary, then save.",
	},
	"google.goals": {
		"Open each campaign named here in Google Ads and go to Settings, Goals.",
		"Add the goal named here and save.",
	},
	"google.status": {
		"Open the page and find the campaign or ad named here.",
		"Point at its status to read what Google asks for.",
		"Edit the ad, or press Appeal if you think Google is wrong.",
	},
	"google.customer_list": {
		"In Google Ads open Tools, Shared library, Audience manager, then Your data segments.",
		"Press plus, Customer list, and upload only people who agreed to their details being used for ads.",
	},
	"meta.account": {
		"Open the page and read the notice about this ad account.",
		"Pay what is owed, or press Request review.",
		"Come back here and press Check again.",
	},
	"meta.funding": {
		"Open Meta billing and pick this ad account.",
		"Under Payment methods press Add payment method.",
		"If you pay before ads run, also press Add funds.",
	},
	"meta.spend_cap": {
		"Open Meta billing and pick this ad account.",
		"Find Account spending limit and press Edit.",
		"Type the most this account may spend and save.",
	},
	"meta.pixel": {
		"Open Events Manager and pick the pixel named here.",
		"Press Set up, then Install the code on your website, and follow the steps.",
		"Open your website once, then press Check again here.",
	},
	"meta.conversions_api": {
		"Ask for both an email and a phone number on your forms.",
		"Keep sending leads for a few days, then press Check again.",
	},
	"meta.lead_form": {
		"Open the page with the Facebook login that manages your Page.",
		"Choose your Page and press Accept.",
		"Come back here and press Check again, then create the form.",
	},
	"meta.instagram": {
		"Open the page with the Facebook login that manages your Page.",
		"Press Connect account and sign in to your Instagram professional account.",
		"In MarketFlow, Settings, Integrations, connect Meta again and tick it.",
	},
	"meta.whatsapp": {
		"Open the page with the Facebook login that manages your Page.",
		"Type your business WhatsApp number and enter the code WhatsApp sends you.",
	},
	"meta.domain": {
		"Open Meta Business settings, Brand safety, Domains and press Add.",
		"Type your website's domain, without https:// or www.",
		"Choose the DNS option and copy the TXT record Meta shows.",
		"Add it where your domain is managed, wait a few minutes, then press Verify domain.",
	},
	"meta.policy": {
		"Open Ads Manager and find the ad named here.",
		"Read why Meta flagged it, then edit the ad or press Request review.",
	},
}

// withSteps adds the manual steps to every item that is not done and has no fix or steps of its own.
func withSteps(items []Item) []Item {
	for i := range items {
		it := &items[i]
		if it.Status == StatusOK || it.Fix != nil || len(it.Steps) > 0 {
			continue
		}
		if it.Status == StatusInfo && it.Link == nil {
			continue
		}
		key, _, _ := strings.Cut(it.Key, ":")
		it.Steps = manualSteps[key]
	}
	return items
}
