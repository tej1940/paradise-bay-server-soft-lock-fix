package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func newWorldMarketTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := OpenDB(filepath.Join(t.TempDir(), "market.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return store
}

func TestWorldMarketViewsExposeOffersForSelectedSeller(t *testing.T) {
	store := newWorldMarketTestStore(t)
	player, err := store.CreatePlayer("market-views-player", "Island", "market-device")
	if err != nil {
		t.Fatal(err)
	}

	fullView, err := store.worldMarketFullViewResponse(player.UUID)
	if err != nil {
		t.Fatalf("build full world market view: %v", err)
	}
	if len(fullView.Markets) != worldMarketOfferCount {
		t.Fatalf("full market count = %d, want %d", len(fullView.Markets), worldMarketOfferCount)
	}

	var fullDescription worldMarketDescription
	if err := json.Unmarshal([]byte(fullView.Markets[0]), &fullDescription); err != nil {
		t.Fatalf("decode full market description: %v", err)
	}
	sellerUUID := fullDescription.PlayerID
	if sellerUUID == "" {
		t.Fatal("full market description has an empty seller ID")
	}
	if len(fullDescription.Sales) != 1 {
		t.Fatalf("seller has %d sales, want 1", len(fullDescription.Sales))
	}

	response, err := store.worldMarketViewsResponse(player.UUID, map[string]interface{}{
		"players": []interface{}{sellerUUID},
	})
	if err != nil {
		t.Fatalf("build selected seller view: %v", err)
	}
	if len(response.Markets) != 1 {
		t.Fatalf("selected market count = %d, want 1", len(response.Markets))
	}
	var selected worldMarketDescription
	if err := json.Unmarshal([]byte(response.Markets[0]), &selected); err != nil {
		t.Fatalf("decode selected market description: %v", err)
	}
	if len(selected.Sales) != 1 {
		t.Fatalf("selected seller has %d sales, want 1", len(selected.Sales))
	}
	if selected.Sales[0].ID != fullDescription.Sales[0].ID ||
		selected.Sales[0].Resource != fullDescription.Sales[0].Resource {
		t.Fatalf("selected seller sales do not match full view: %+v vs %+v", selected.Sales[0], fullDescription.Sales[0])
	}
	if selected.Alias != fullDescription.Alias || selected.IconURL != fullDescription.IconURL {
		t.Fatalf("selected seller identity does not match full view: %+v vs %+v", selected, fullDescription)
	}

	numericResponse, err := store.worldMarketViewsResponse(player.UUID, map[string]interface{}{
		"players": []interface{}{float64(0)},
	})
	if err != nil {
		t.Fatalf("build numeric selected seller view: %v", err)
	}
	var numericSelected worldMarketDescription
	if len(numericResponse.Markets) != 1 {
		t.Fatalf("numeric selected market count = %d, want 1", len(numericResponse.Markets))
	}
	if err := json.Unmarshal([]byte(numericResponse.Markets[0]), &numericSelected); err != nil {
		t.Fatalf("decode numeric selected market: %v", err)
	}
	if numericSelected.Sales[0].ID != fullDescription.Sales[0].ID {
		t.Fatalf("numeric seller index 0 selected sale %q, want %q", numericSelected.Sales[0].ID, fullDescription.Sales[0].ID)
	}

	secondNumericResponse, err := store.worldMarketViewsResponse(player.UUID, map[string]interface{}{
		"players": []interface{}{float64(1)},
	})
	if err != nil {
		t.Fatalf("build second numeric selected seller view: %v", err)
	}
	var secondNumericSelected worldMarketDescription
	if err := json.Unmarshal([]byte(secondNumericResponse.Markets[0]), &secondNumericSelected); err != nil {
		t.Fatalf("decode second numeric selected market: %v", err)
	}
	var secondFullDescription worldMarketDescription
	if err := json.Unmarshal([]byte(fullView.Markets[1]), &secondFullDescription); err != nil {
		t.Fatalf("decode second full market description: %v", err)
	}
	if secondNumericSelected.Sales[0].ID != secondFullDescription.Sales[0].ID {
		t.Fatalf("numeric seller index 1 selected sale %q, want %q", secondNumericSelected.Sales[0].ID, secondFullDescription.Sales[0].ID)
	}

	commandResult := resolveServerCommand(store, player.UUID, "getFullWorldMarketView", nil)
	var commandPayload worldMarketViewsPayload
	if err := json.Unmarshal([]byte(commandResult), &commandPayload); err != nil {
		t.Fatalf("decode getFullWorldMarketView command result: %v", err)
	}
	if len(commandPayload.Markets) != worldMarketOfferCount {
		t.Fatalf("command market count = %d, want %d", len(commandPayload.Markets), worldMarketOfferCount)
	}
}

func TestWorldMarketCacheIsGeneratedForNewPlayer(t *testing.T) {
	store := newWorldMarketTestStore(t)
	player, err := store.CreatePlayer("market-player", "Island", "market-device")
	if err != nil {
		t.Fatal(err)
	}

	blob, err := store.GetFragment(worldMarketCacheKey(player.UUID))
	if err != nil {
		t.Fatalf("get generated world market cache: %v", err)
	}

	var cache worldMarketCacheFragment
	if err := json.Unmarshal([]byte(blob), &cache); err != nil {
		t.Fatalf("decode generated world market cache: %v", err)
	}
	if cache.Type != worldMarketCacheType {
		t.Fatalf("cache type = %q, want %q", cache.Type, worldMarketCacheType)
	}
	if len(cache.View) != worldMarketOfferCount {
		t.Fatalf("offer count = %d, want %d", len(cache.View), worldMarketOfferCount)
	}
	if cache.NextRefreshTimeMillis <= time.Now().UnixMilli() {
		t.Fatalf("next refresh time %d is not in the future", cache.NextRefreshTimeMillis)
	}

	seen := make(map[string]bool, len(cache.View))
	for i, rawView := range cache.View {
		var view worldMarketCachedView
		if err := json.Unmarshal(rawView, &view); err != nil {
			t.Fatalf("decode market view %d: %v", i, err)
		}
		if view.UUID == "" || len(view.Slots) != 1 || len(view.Sales) != 1 {
			t.Fatalf("market view %d has an invalid seller or sale list: %+v", i, view)
		}
		saleID := view.Slots[0]
		sale, ok := view.Sales[saleID]
		if !ok || sale.Resource == "" || sale.Amount < 1 || sale.Price < 1 || sale.State != "available" {
			t.Fatalf("market view %d has invalid purchase data: %+v", i, view)
		}
		if view.UUID != worldMarketSellerUUID(player.UUID, saleID) {
			t.Fatalf("market view %d seller UUID does not match sale %q", i, saleID)
		}
		if seen[sale.Resource] {
			t.Fatalf("duplicate market item %q", sale.Resource)
		}
		seen[sale.Resource] = true
	}
}

func TestWorldMarketCacheRefreshesWhenExpired(t *testing.T) {
	store := newWorldMarketTestStore(t)
	player, err := store.CreatePlayer("market-expired", "Island", "market-device")
	if err != nil {
		t.Fatal(err)
	}

	key := worldMarketCacheKey(player.UUID)
	blob, err := store.GetFragment(key)
	if err != nil {
		t.Fatal(err)
	}
	var stale worldMarketCacheFragment
	if err := json.Unmarshal([]byte(blob), &stale); err != nil {
		t.Fatal(err)
	}
	stale.NextRefreshTimeMillis = time.Now().Add(-time.Minute).UnixMilli()
	staleBlob, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFragment(key, string(staleBlob), "stale-hash"); err != nil {
		t.Fatal(err)
	}

	fragments, err := store.GetFragmentsForDocument("GamePlayer:P[" + player.UUID + "]")
	if err != nil {
		t.Fatalf("request GamePlayer fragments: %v", err)
	}
	var refreshed worldMarketCacheFragment
	if err := json.Unmarshal([]byte(fragments[key]), &refreshed); err != nil {
		t.Fatalf("decode refreshed world market cache: %v", err)
	}
	if refreshed.Token != stale.Token+1 {
		t.Fatalf("refreshed token = %d, want %d", refreshed.Token, stale.Token+1)
	}
	if refreshed.NextRefreshTimeMillis <= time.Now().UnixMilli() {
		t.Fatalf("refreshed cache expires immediately: %d", refreshed.NextRefreshTimeMillis)
	}
	if len(refreshed.View) != worldMarketOfferCount {
		t.Fatalf("refreshed offer count = %d, want %d", len(refreshed.View), worldMarketOfferCount)
	}
	var refreshedView worldMarketCachedView
	if err := json.Unmarshal(refreshed.View[0], &refreshedView); err != nil {
		t.Fatalf("decode refreshed market view: %v", err)
	}
	if refreshedView.UUID == "" || len(refreshedView.Slots) != 1 {
		t.Fatalf("refreshed cache contains a non-client market view: %+v", refreshedView)
	}
}

func TestWorldMarketCacheNormalizesLegacyOffers(t *testing.T) {
	store := newWorldMarketTestStore(t)
	player, err := store.CreatePlayer("market-legacy-cache", "Island", "market-legacy-device")
	if err != nil {
		t.Fatal(err)
	}

	key := worldMarketCacheKey(player.UUID)
	blob, err := store.GetFragment(key)
	if err != nil {
		t.Fatal(err)
	}
	var cache worldMarketCacheFragment
	if err := json.Unmarshal([]byte(blob), &cache); err != nil {
		t.Fatal(err)
	}
	for i, rawView := range cache.View {
		var view worldMarketCachedView
		if err := json.Unmarshal(rawView, &view); err != nil {
			t.Fatal(err)
		}
		saleID := view.Slots[0]
		sale := view.Sales[saleID]
		legacyOffer, err := json.Marshal(worldMarketOffer{
			SaleID:     saleID,
			SellerName: view.BestAlias,
			ItemKey:    sale.Resource,
			Amount:     sale.Amount,
			Price:      sale.Price,
		})
		if err != nil {
			t.Fatal(err)
		}
		cache.View[i] = legacyOffer
	}
	token := cache.Token
	nextRefresh := cache.NextRefreshTimeMillis
	legacyBlob, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFragment(key, string(legacyBlob), "legacy-hash"); err != nil {
		t.Fatal(err)
	}

	if err := store.ensureWorldMarketCache(player.UUID); err != nil {
		t.Fatalf("normalize legacy world market cache: %v", err)
	}
	normalizedBlob, err := store.GetFragment(key)
	if err != nil {
		t.Fatal(err)
	}
	var normalized worldMarketCacheFragment
	if err := json.Unmarshal([]byte(normalizedBlob), &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized.Token != token+1 || normalized.NextRefreshTimeMillis != nextRefresh {
		t.Fatalf("normalized cache token/expiry = %d/%d, want %d/%d", normalized.Token, normalized.NextRefreshTimeMillis, token+1, nextRefresh)
	}
	for i, rawView := range normalized.View {
		var view worldMarketCachedView
		if err := json.Unmarshal(rawView, &view); err != nil {
			t.Fatalf("decode normalized view %d: %v", i, err)
		}
		if view.UUID == "" || len(view.Slots) != 1 {
			t.Fatalf("view %d was not normalized: %+v", i, view)
		}
	}
}

func TestWorldMarketPurchaseCompletesOnce(t *testing.T) {
	store := newWorldMarketTestStore(t)
	player, err := store.CreatePlayer("market-buy-player", "Island", "market-buy-device")
	if err != nil {
		t.Fatal(err)
	}

	cacheKey := worldMarketCacheKey(player.UUID)
	blob, err := store.GetFragment(cacheKey)
	if err != nil {
		t.Fatal(err)
	}
	var cache worldMarketCacheFragment
	if err := json.Unmarshal([]byte(blob), &cache); err != nil {
		t.Fatalf("decode market cache: %v", err)
	}
	var cachedView worldMarketCachedView
	if err := json.Unmarshal(cache.View[0], &cachedView); err != nil {
		t.Fatalf("decode cached market view: %v", err)
	}
	if len(cachedView.Slots) != 1 {
		t.Fatalf("cached view has %d slots, want 1", len(cachedView.Slots))
	}
	saleID := cachedView.Slots[0]
	cachedSale := cachedView.Sales[saleID]
	sellerUUID := cachedView.UUID
	offer := worldMarketOffer{
		SaleID:     saleID,
		SellerName: cachedView.BestAlias,
		ItemKey:    cachedSale.Resource,
		Amount:     cachedSale.Amount,
		Price:      cachedSale.Price,
	}

	args := []interface{}{
		sellerUUID,
		offer.SaleID,
		"Buyer",
		"buyer-icon",
		map[string]interface{}{
			"resource":      offer.ItemKey,
			"amount":        float64(offer.Amount),
			"price":         float64(offer.Price),
			"priceResource": "resource_gold",
			"state":         "pending",
		},
		false,
	}
	if _, err := store.completeWorldMarketPurchase(player.UUID, []interface{}{
		sellerUUID,
		offer.SaleID,
		"Buyer",
		"buyer-icon",
		map[string]interface{}{
			"resource":      offer.ItemKey,
			"amount":        float64(offer.Amount),
			"price":         float64(offer.Price + 1),
			"priceResource": "resource_gold",
		},
		false,
	}); err == nil {
		t.Fatal("purchase with a mismatched price unexpectedly succeeded")
	}

	updates, err := store.completeWorldMarketPurchase(player.UUID, args)
	if err != nil {
		t.Fatalf("complete market purchase: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("purchase returned %d fragment updates, want market and mail", len(updates))
	}

	listings, err := store.getWorldMarketListings(player.UUID)
	if err != nil {
		t.Fatalf("load updated market listings: %v", err)
	}
	if listings[0].Sale.State != "bought" ||
		len(listings[0].Sale.BoughtBy) != 1 ||
		listings[0].Sale.BoughtBy[0] != player.UUID {
		t.Fatalf("purchased sale state = %+v", listings[0].Sale)
	}
	fullView, err := store.worldMarketFullViewResponse(player.UUID)
	if err != nil {
		t.Fatalf("build market view after purchase: %v", err)
	}
	var purchasedView worldMarketDescription
	if err := json.Unmarshal([]byte(fullView.Markets[0]), &purchasedView); err != nil {
		t.Fatalf("decode market view after purchase: %v", err)
	}
	if purchasedView.Sales[0].Buyer != player.UUID {
		t.Fatalf("market view buyer = %q, want %q", purchasedView.Sales[0].Buyer, player.UUID)
	}

	mailBlob, err := store.GetFragment(worldMarketMailKey(player.UUID))
	if err != nil {
		t.Fatalf("load purchase mail: %v", err)
	}
	var mailFragment worldMarketMailFragment
	if err := json.Unmarshal([]byte(mailBlob), &mailFragment); err != nil {
		t.Fatalf("decode purchase mail fragment: %v", err)
	}
	mails, err := decodeWorldMarketMailMap(mailFragment.MailByID)
	if err != nil {
		t.Fatalf("decode purchase mails: %v", err)
	}
	if mailFragment.MailIDCounter != 1 || len(mails) != 1 {
		t.Fatalf("mail counter = %d, mail count = %d; want 1 each", mailFragment.MailIDCounter, len(mails))
	}
	var purchaseMail map[string]interface{}
	if err := json.Unmarshal(mails["0"], &purchaseMail); err != nil {
		t.Fatalf("decode market purchase mail: %v", err)
	}
	if purchaseMail["type"] != "TKMarketBuyComplete" ||
		purchaseMail["resource"] != offer.ItemKey ||
		purchaseMail["saleId"] != offer.SaleID {
		t.Fatalf("purchase mail has unexpected content: %+v", purchaseMail)
	}

	duplicateUpdates, err := store.completeWorldMarketPurchase(player.UUID, args)
	if err != nil {
		t.Fatalf("repeat market purchase: %v", err)
	}
	if len(duplicateUpdates) != 0 {
		t.Fatalf("repeat purchase returned %d updates, want none", len(duplicateUpdates))
	}
	mailBlob, err = store.GetFragment(worldMarketMailKey(player.UUID))
	if err != nil {
		t.Fatalf("reload purchase mail: %v", err)
	}
	if err := json.Unmarshal([]byte(mailBlob), &mailFragment); err != nil {
		t.Fatalf("decode reloaded mail fragment: %v", err)
	}
	mails, err = decodeWorldMarketMailMap(mailFragment.MailByID)
	if err != nil {
		t.Fatalf("decode reloaded mails: %v", err)
	}
	if mailFragment.MailIDCounter != 1 || len(mails) != 1 {
		t.Fatalf("repeat purchase created duplicate mail: counter=%d count=%d", mailFragment.MailIDCounter, len(mails))
	}
}
