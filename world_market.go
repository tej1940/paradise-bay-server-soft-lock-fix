package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

const (
	worldMarketCacheType                     = "TKWorldMarketCacheFragment:v1"
	worldMarketOfferCount                    = 14
	worldMarketDefaultRefreshIntervalMinutes = 5
	worldMarketMinRefreshIntervalMinutes     = 1
	worldMarketMaxRefreshIntervalMinutes     = 1440
	worldMarketOfferLifetime                 = 48 * time.Hour
	worldMarketSettingsKey                   = "default"
)

type worldMarketSettings struct {
	RefreshIntervalMinutes int `json:"refreshIntervalMinutes"`
	OfferCount             int `json:"offerCount"`
}

type worldMarketCacheFragment struct {
	Type                  string            `json:"_t"`
	NextRefreshTimeMillis int64             `json:"nextRefreshTimeMillis"`
	Token                 int64             `json:"token"`
	View                  []json.RawMessage `json:"view"`
}

type worldMarketOffer struct {
	SaleID           string `json:"saleId"`
	SellerName       string `json:"sellerName"`
	ItemKey          string `json:"itemKey"`
	Amount           int    `json:"amount"`
	Price            int    `json:"price"`
	ExpiryTimeMillis int64  `json:"expiryTimeMillis"`
}

type worldMarketCachedSale struct {
	Amount        int      `json:"amount"`
	BoughtBy      []string `json:"boughtBy"`
	Limited       bool     `json:"limited"`
	Price         int      `json:"price"`
	PriceResource string   `json:"priceResource"`
	Resource      string   `json:"resource"`
	State         string   `json:"state"`
}

type worldMarketCachedView struct {
	BestAlias     string                           `json:"bestAlias"`
	FeaturedSale  string                           `json:"featuredSale,omitempty"`
	FeaturedSlot  int                              `json:"featuredSlot"`
	FetchedTimeMs int64                            `json:"fetchedTimeMs"`
	IconURL       string                           `json:"iconUrl"`
	IsFriend      bool                             `json:"isFriend"`
	Sales         map[string]worldMarketCachedSale `json:"sales"`
	Slots         []string                         `json:"slots"`
	UUID          string                           `json:"uuid"`
}

type worldMarketListing struct {
	UUID     string
	Alias    string
	IconURL  string
	IsFriend bool
	SaleID   string
	Sale     worldMarketCachedSale
}

type worldMarketViewsPayload struct {
	Markets []string `json:"markets"`
}

type worldMarketDescription struct {
	PlayerID            string                       `json:"p"`
	TimeMillis          int64                        `json:"t"`
	Sales               []worldMarketSaleDescription `json:"s"`
	Alias               string                       `json:"a"`
	IconURL             string                       `json:"i"`
	OfflineHackIsFriend bool                         `json:"offlineHackIsFriend"`
}

type worldMarketSaleDescription struct {
	ID       string `json:"i"`
	Resource string `json:"r"`
	Amount   int    `json:"a"`
	Price    int    `json:"p"`
	Limited  bool   `json:"l"`
	Buyer    string `json:"b,omitempty"`
}

type worldMarketPurchaseUpdate struct {
	Key      string
	BlobJSON string
}

type worldMarketMailFragment struct {
	Type          string          `json:"_t"`
	MailByID      json.RawMessage `json:"mailById"`
	MailIDCounter int64           `json:"mailIdCounter"`
}

func (s *Store) worldMarketFullViewResponse(playerUUID string) (worldMarketViewsPayload, error) {
	listings, err := s.getWorldMarketListings(playerUUID)
	if err != nil {
		return worldMarketViewsPayload{}, err
	}

	markets := make([]string, 0, len(listings))
	now := time.Now().UnixMilli()
	for _, listing := range listings {
		description := worldMarketDescription{
			PlayerID:            listing.UUID,
			TimeMillis:          now,
			Sales:               []worldMarketSaleDescription{worldMarketSaleFromListing(listing)},
			Alias:               listing.Alias,
			IconURL:             listing.IconURL,
			OfflineHackIsFriend: listing.IsFriend,
		}
		blob, err := json.Marshal(description)
		if err != nil {
			return worldMarketViewsPayload{}, fmt.Errorf("encode world market view for seller %s: %w", listing.UUID, err)
		}
		markets = append(markets, string(blob))
	}
	return worldMarketViewsPayload{Markets: markets}, nil
}

func (s *Store) worldMarketViewsResponse(playerUUID string, requestData any) (worldMarketViewsPayload, error) {
	if playerUUID == "" {
		return worldMarketViewsPayload{}, fmt.Errorf("world market request has no connected player")
	}
	data, ok := requestData.(map[string]interface{})
	if !ok {
		return worldMarketViewsPayload{}, fmt.Errorf("world market request has invalid data payload %T", requestData)
	}
	players, ok := data["players"].([]interface{})
	if !ok {
		return worldMarketViewsPayload{}, fmt.Errorf("world market request is missing a players list")
	}
	if len(players) > 64 {
		return worldMarketViewsPayload{}, fmt.Errorf("world market request has %d players; maximum is 64", len(players))
	}

	listings, err := s.getWorldMarketListings(playerUUID)
	if err != nil {
		return worldMarketViewsPayload{}, err
	}
	listingsBySeller := make(map[string]worldMarketListing, len(listings))
	for _, listing := range listings {
		listingsBySeller[listing.UUID] = listing
	}

	markets := make([]string, 0, len(players))
	for _, value := range players {
		var listing worldMarketListing
		switch id := value.(type) {
		case string:
			if id == "" {
				return worldMarketViewsPayload{}, fmt.Errorf("world market request contains an empty player ID")
			}
			var found bool
			listing, found = listingsBySeller[id]
			if !found {
				continue
			}
		case float64:
			if math.IsNaN(id) || math.IsInf(id, 0) || math.Trunc(id) != id || id < 0 || id >= float64(len(listings)) {
				return worldMarketViewsPayload{}, fmt.Errorf("world market request contains unsupported numeric player ID %v", id)
			}
			listing = listings[int(id)]
		default:
			return worldMarketViewsPayload{}, fmt.Errorf("world market request contains invalid player ID %T", value)
		}

		description := worldMarketDescription{
			PlayerID:            listing.UUID,
			TimeMillis:          time.Now().UnixMilli(),
			Sales:               []worldMarketSaleDescription{worldMarketSaleFromListing(listing)},
			Alias:               listing.Alias,
			IconURL:             listing.IconURL,
			OfflineHackIsFriend: listing.IsFriend,
		}
		blob, err := json.Marshal(description)
		if err != nil {
			return worldMarketViewsPayload{}, fmt.Errorf("encode world market view for seller %s: %w", listing.UUID, err)
		}
		markets = append(markets, string(blob))
	}
	return worldMarketViewsPayload{Markets: markets}, nil
}

func worldMarketSellerUUID(playerUUID, saleID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("paradise-bay:world-market:"+playerUUID+":"+saleID)).String()
}

func worldMarketSaleFromListing(listing worldMarketListing) worldMarketSaleDescription {
	description := worldMarketSaleDescription{
		ID:       listing.SaleID,
		Resource: listing.Sale.Resource,
		Amount:   listing.Sale.Amount,
		Price:    listing.Sale.Price,
		Limited:  listing.Sale.Limited,
	}
	if len(listing.Sale.BoughtBy) > 0 {
		description.Buyer = listing.Sale.BoughtBy[0]
	}
	return description
}

func (s *Store) getWorldMarketListings(playerUUID string) ([]worldMarketListing, error) {
	if err := s.ensureWorldMarketCache(playerUUID); err != nil {
		return nil, fmt.Errorf("ensure world market cache for player %s: %w", playerUUID, err)
	}
	blob, err := s.GetFragment(worldMarketCacheKey(playerUUID))
	if err != nil {
		return nil, fmt.Errorf("load world market cache for player %s: %w", playerUUID, err)
	}

	var cache worldMarketCacheFragment
	if err := json.Unmarshal([]byte(blob), &cache); err != nil {
		return nil, fmt.Errorf("decode world market cache for player %s: %w", playerUUID, err)
	}
	if cache.Type != worldMarketCacheType {
		return nil, fmt.Errorf("world market cache for player %s has unexpected type %q", playerUUID, cache.Type)
	}
	listings := make([]worldMarketListing, 0, len(cache.View))
	for i, rawView := range cache.View {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawView, &fields); err != nil {
			return nil, fmt.Errorf("decode world market view %d for player %s: %w", i, playerUUID, err)
		}
		if _, isOffer := fields["saleId"]; isOffer {
			var offer worldMarketOffer
			if err := json.Unmarshal(rawView, &offer); err != nil {
				return nil, fmt.Errorf("decode world market offer %d for player %s: %w", i, playerUUID, err)
			}
			listings = append(listings, worldMarketListing{
				UUID:    worldMarketSellerUUID(playerUUID, offer.SaleID),
				Alias:   offer.SellerName,
				IconURL: fmt.Sprintf("random player icon %d", i+1),
				SaleID:  offer.SaleID,
				Sale: worldMarketCachedSale{
					Amount:        offer.Amount,
					Limited:       false,
					Price:         offer.Price,
					PriceResource: "resource_gold",
					Resource:      offer.ItemKey,
					State:         "available",
				},
			})
			continue
		}

		var cachedView worldMarketCachedView
		if err := json.Unmarshal(rawView, &cachedView); err != nil {
			return nil, fmt.Errorf("decode cached world market view %d for player %s: %w", i, playerUUID, err)
		}
		if cachedView.UUID == "" {
			return nil, fmt.Errorf("cached world market view %d for player %s has no UUID", i, playerUUID)
		}
		for _, saleID := range cachedView.Slots {
			if saleID == "" {
				continue
			}
			sale, ok := cachedView.Sales[saleID]
			if !ok {
				continue
			}
			listings = append(listings, worldMarketListing{
				UUID:     cachedView.UUID,
				Alias:    cachedView.BestAlias,
				IconURL:  cachedView.IconURL,
				IsFriend: cachedView.IsFriend,
				SaleID:   saleID,
				Sale:     sale,
			})
		}
	}
	return listings, nil
}

func (s *Store) completeWorldMarketPurchase(buyerUUID string, args []interface{}) ([]worldMarketPurchaseUpdate, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("buyFromPlayer has %d arguments; want at least 5", len(args))
	}
	sellerUUID, ok := args[0].(string)
	if !ok || sellerUUID == "" {
		return nil, fmt.Errorf("buyFromPlayer has invalid seller UUID %T", args[0])
	}
	saleID, ok := args[1].(string)
	if !ok || saleID == "" {
		return nil, fmt.Errorf("buyFromPlayer has invalid sale ID %T", args[1])
	}
	if buyerUUID == "" || sellerUUID != worldMarketSellerUUID(buyerUUID, saleID) {
		return nil, nil
	}
	saleData, ok := args[4].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("buyFromPlayer has invalid sale data %T", args[4])
	}
	resource, ok := saleData["resource"].(string)
	if !ok || resource == "" {
		return nil, fmt.Errorf("buyFromPlayer sale data has invalid resource")
	}
	priceResource := "resource_gold"
	if rawPriceResource, exists := saleData["priceResource"]; exists {
		value, ok := rawPriceResource.(string)
		if !ok || (value != "" && value != priceResource) {
			return nil, fmt.Errorf("buyFromPlayer sale data has invalid price resource")
		}
	}
	amount, ok := integerTransactionValue(saleData["amount"])
	if !ok || amount < 1 {
		return nil, fmt.Errorf("buyFromPlayer sale data has invalid amount")
	}
	price, ok := integerTransactionValue(saleData["price"])
	if !ok || price < 1 {
		return nil, fmt.Errorf("buyFromPlayer sale data has invalid price")
	}

	var updates []worldMarketPurchaseUpdate
	err := s.db.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
		cacheKey := worldMarketCacheKey(buyerUUID)
		var cacheFragment DocumentFragment
		if err := tx.NewSelect().Model(&cacheFragment).Where("key = ?", cacheKey).Scan(ctx); err != nil {
			return fmt.Errorf("load market cache for purchase: %w", err)
		}
		var cache worldMarketCacheFragment
		if err := json.Unmarshal([]byte(cacheFragment.BlobJSON), &cache); err != nil {
			return fmt.Errorf("decode market cache for purchase: %w", err)
		}
		if cache.Type != worldMarketCacheType {
			return fmt.Errorf("market cache for purchase has unexpected type %q", cache.Type)
		}

		found, alreadyBought := false, false
		for i, rawView := range cache.View {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rawView, &fields); err != nil {
				return fmt.Errorf("decode market view %d for purchase: %w", i, err)
			}
			if _, isOffer := fields["saleId"]; isOffer {
				var offer worldMarketOffer
				if err := json.Unmarshal(rawView, &offer); err != nil {
					return fmt.Errorf("decode offer %d for purchase: %w", i, err)
				}
				if offer.SaleID != saleID {
					continue
				}
				if offer.ItemKey != resource || offer.Amount != amount || offer.Price != price {
					return fmt.Errorf("buyFromPlayer data does not match simulated sale %s", saleID)
				}
				cachedView := worldMarketCachedView{
					BestAlias:     offer.SellerName,
					FeaturedSale:  saleID,
					FeaturedSlot:  1,
					FetchedTimeMs: time.Now().UnixMilli(),
					IconURL:       fmt.Sprintf("random player icon %d", i+1),
					Sales: map[string]worldMarketCachedSale{
						saleID: {
							Amount:        offer.Amount,
							BoughtBy:      []string{buyerUUID},
							Price:         offer.Price,
							PriceResource: "resource_gold",
							Resource:      offer.ItemKey,
							State:         "bought",
						},
					},
					Slots: []string{saleID},
					UUID:  sellerUUID,
				}
				updatedView, err := json.Marshal(cachedView)
				if err != nil {
					return fmt.Errorf("encode purchased world market view: %w", err)
				}
				cache.View[i] = updatedView
				found = true
				break
			}

			var cachedSellerUUID string
			if err := json.Unmarshal(fields["uuid"], &cachedSellerUUID); err != nil {
				return fmt.Errorf("decode cached seller UUID for purchase: %w", err)
			}
			if cachedSellerUUID != sellerUUID {
				continue
			}
			var sales map[string]json.RawMessage
			if err := json.Unmarshal(fields["sales"], &sales); err != nil {
				return fmt.Errorf("decode sales for cached seller %s: %w", sellerUUID, err)
			}
			rawSale, exists := sales[saleID]
			if !exists {
				continue
			}
			var sale worldMarketCachedSale
			if err := json.Unmarshal(rawSale, &sale); err != nil {
				return fmt.Errorf("decode sale %s for cached seller %s: %w", saleID, sellerUUID, err)
			}
			found = true
			if sale.State == "bought" {
				alreadyBought = true
				break
			}
			if (sale.State != "" && sale.State != "available" && sale.State != "pending") ||
				sale.Resource != resource || sale.Amount != amount || sale.Price != price ||
				(sale.PriceResource != "" && sale.PriceResource != priceResource) {
				return fmt.Errorf("buyFromPlayer data does not match available simulated sale %s", saleID)
			}
			sale.State = "bought"
			sale.BoughtBy = []string{buyerUUID}
			encodedSale, err := json.Marshal(sale)
			if err != nil {
				return fmt.Errorf("encode purchased sale %s: %w", saleID, err)
			}
			sales[saleID] = encodedSale
			fields["sales"], err = json.Marshal(sales)
			if err != nil {
				return fmt.Errorf("encode sales for cached seller %s: %w", sellerUUID, err)
			}
			updatedView, err := json.Marshal(fields)
			if err != nil {
				return fmt.Errorf("encode purchased world market view: %w", err)
			}
			cache.View[i] = updatedView
			break
		}
		if !found {
			return fmt.Errorf("simulated seller %s or sale %s is not in the market cache", sellerUUID, saleID)
		}
		if alreadyBought {
			return nil
		}

		cache.Token++
		cacheBlob, err := json.Marshal(cache)
		if err != nil {
			return fmt.Errorf("encode updated world market cache: %w", err)
		}
		if err := upsertDocumentFragmentInTx(ctx, tx, cacheKey, string(cacheBlob)); err != nil {
			return fmt.Errorf("store updated world market cache: %w", err)
		}

		mailKey := worldMarketMailKey(buyerUUID)
		mailFragment, err := loadWorldMarketMailFragment(ctx, tx, mailKey)
		if err != nil {
			return err
		}
		mailByID, err := decodeWorldMarketMailMap(mailFragment.MailByID)
		if err != nil {
			return fmt.Errorf("decode mail fragment %s: %w", mailKey, err)
		}
		mailID := strconv.FormatInt(mailFragment.MailIDCounter, 10)
		mail, err := json.Marshal(map[string]interface{}{
			"type":                "TKMarketBuyComplete",
			"saleId":              saleID,
			"sellerUuid":          sellerUUID,
			"resource":            resource,
			"amount":              amount,
			"wealthDepositAction": "Collect from world market purchase",
		})
		if err != nil {
			return fmt.Errorf("encode market purchase mail: %w", err)
		}
		mailByID[mailID] = mail
		mailFragment.MailByID, err = json.Marshal(mailByID)
		if err != nil {
			return fmt.Errorf("encode mail fragment %s: %w", mailKey, err)
		}
		mailFragment.MailIDCounter++
		mailBlob, err := json.Marshal(mailFragment)
		if err != nil {
			return fmt.Errorf("encode mail fragment %s: %w", mailKey, err)
		}
		if err := upsertDocumentFragmentInTx(ctx, tx, mailKey, string(mailBlob)); err != nil {
			return fmt.Errorf("store market purchase mail: %w", err)
		}

		updates = []worldMarketPurchaseUpdate{
			{Key: cacheKey, BlobJSON: string(cacheBlob)},
			{Key: mailKey, BlobJSON: string(mailBlob)},
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("complete simulated market purchase: %w", err)
	}
	return updates, nil
}

func defaultWorldMarketSettings() worldMarketSettings {
	return worldMarketSettings{
		RefreshIntervalMinutes: worldMarketDefaultRefreshIntervalMinutes,
		OfferCount:             worldMarketOfferCount,
	}
}

func validateWorldMarketSettings(settings worldMarketSettings) error {
	if settings.RefreshIntervalMinutes < worldMarketMinRefreshIntervalMinutes ||
		settings.RefreshIntervalMinutes > worldMarketMaxRefreshIntervalMinutes {
		return fmt.Errorf(
			"l'intervalle doit être compris entre %d et %d minutes",
			worldMarketMinRefreshIntervalMinutes,
			worldMarketMaxRefreshIntervalMinutes,
		)
	}
	if settings.OfferCount < 1 || settings.OfferCount > len(worldMarketResources) {
		return fmt.Errorf("le nombre d'articles doit être compris entre 1 et %d", len(worldMarketResources))
	}
	return nil
}

func (s *Store) getWorldMarketSettings() (worldMarketSettings, error) {
	var record WorldMarketSettingsRecord
	err := s.db.NewSelect().Model(&record).Where("key = ?", worldMarketSettingsKey).Scan(context.Background())
	if errors.Is(err, sql.ErrNoRows) {
		return defaultWorldMarketSettings(), nil
	}
	if err != nil {
		return worldMarketSettings{}, fmt.Errorf("charger les réglages du marché: %w", err)
	}
	settings := worldMarketSettings{
		RefreshIntervalMinutes: record.RefreshIntervalMinutes,
		OfferCount:             record.OfferCount,
	}
	if err := validateWorldMarketSettings(settings); err != nil {
		return worldMarketSettings{}, fmt.Errorf("les réglages sauvegardés du marché sont invalides: %w", err)
	}
	return settings, nil
}

func (s *Store) setWorldMarketSettings(settings worldMarketSettings) error {
	if err := validateWorldMarketSettings(settings); err != nil {
		return err
	}
	record := WorldMarketSettingsRecord{
		Key:                    worldMarketSettingsKey,
		RefreshIntervalMinutes: settings.RefreshIntervalMinutes,
		OfferCount:             settings.OfferCount,
	}
	_, err := s.db.NewInsert().Model(&record).
		On("CONFLICT (key) DO UPDATE SET refresh_interval_minutes = EXCLUDED.refresh_interval_minutes, offer_count = EXCLUDED.offer_count").
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("enregistrer les réglages du marché: %w", err)
	}
	return nil
}

func getWorldMarketSettingsInTx(ctx context.Context, tx bun.Tx) (worldMarketSettings, error) {
	var record WorldMarketSettingsRecord
	err := tx.NewSelect().Model(&record).Where("key = ?", worldMarketSettingsKey).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultWorldMarketSettings(), nil
	}
	if err != nil {
		return worldMarketSettings{}, fmt.Errorf("charger les réglages du marché: %w", err)
	}
	settings := worldMarketSettings{
		RefreshIntervalMinutes: record.RefreshIntervalMinutes,
		OfferCount:             record.OfferCount,
	}
	if err := validateWorldMarketSettings(settings); err != nil {
		return worldMarketSettings{}, fmt.Errorf("les réglages sauvegardés du marché sont invalides: %w", err)
	}
	return settings, nil
}

func integerTransactionValue(value interface{}) (int, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return 0, false
	}
	return int(number), true
}

func worldMarketMailKey(playerUUID string) string {
	return "TKMail:mail:P[" + playerUUID + "]"
}

func loadWorldMarketMailFragment(ctx context.Context, tx bun.Tx, key string) (worldMarketMailFragment, error) {
	var fragment DocumentFragment
	err := tx.NewSelect().Model(&fragment).Where("key = ?", key).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return worldMarketMailFragment{
			Type:          "TKMailFragment:v1",
			MailByID:      json.RawMessage(`[]`),
			MailIDCounter: 0,
		}, nil
	}
	if err != nil {
		return worldMarketMailFragment{}, fmt.Errorf("load mail fragment %s: %w", key, err)
	}
	var mailFragment worldMarketMailFragment
	if err := json.Unmarshal([]byte(fragment.BlobJSON), &mailFragment); err != nil {
		return worldMarketMailFragment{}, fmt.Errorf("decode mail fragment %s: %w", key, err)
	}
	if mailFragment.Type != "TKMailFragment:v1" {
		return worldMarketMailFragment{}, fmt.Errorf("mail fragment %s has unexpected type %q", key, mailFragment.Type)
	}
	return mailFragment, nil
}

func decodeWorldMarketMailMap(raw json.RawMessage) (map[string]json.RawMessage, error) {
	mailByID := make(map[string]json.RawMessage)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return mailByID, nil
	}
	switch trimmed[0] {
	case '{':
		if err := json.Unmarshal(trimmed, &mailByID); err != nil {
			return nil, err
		}
	case '[':
		var empty []json.RawMessage
		if err := json.Unmarshal(trimmed, &empty); err != nil {
			return nil, err
		}
		if len(empty) != 0 {
			return nil, fmt.Errorf("expected empty array for mailById, got %d entries", len(empty))
		}
	default:
		return nil, fmt.Errorf("mailById has invalid JSON shape")
	}
	return mailByID, nil
}

func upsertDocumentFragmentInTx(ctx context.Context, tx bun.Tx, key, blob string) error {
	hash := sha256.Sum256([]byte(blob))
	fragment := DocumentFragment{
		Key:       key,
		BlobJSON:  blob,
		Hash:      fmt.Sprintf("%x", hash),
		UpdatedAt: time.Now().UTC().Format("2006-01-02 15:04:05"),
	}
	_, err := tx.NewInsert().Model(&fragment).
		On("CONFLICT (key) DO UPDATE SET blob_json = EXCLUDED.blob_json, hash = EXCLUDED.hash, updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

type worldMarketResource struct {
	key       string
	rarity    int
	maxAmount int
	priceLow  int
	priceHigh int
}

var worldMarketResources = []worldMarketResource{
	{key: "resource_bamboo", rarity: 0, maxAmount: 10, priceLow: 1, priceHigh: 3},
	{key: "resource_cotton", rarity: 0, maxAmount: 10, priceLow: 1, priceHigh: 2},
	{key: "resource_fish", rarity: 0, maxAmount: 10, priceLow: 8, priceHigh: 28},
	{key: "resource_fishingNet", rarity: 0, maxAmount: 10, priceLow: 5, priceHigh: 16},
	{key: "resource_coffee", rarity: 0, maxAmount: 10, priceLow: 3, priceHigh: 8},
	{key: "resource_rope", rarity: 0, maxAmount: 10, priceLow: 5, priceHigh: 16},
	{key: "resource_board", rarity: 0, maxAmount: 10, priceLow: 6, priceHigh: 18},
	{key: "resource_pineapple", rarity: 0, maxAmount: 10, priceLow: 2, priceHigh: 6},
	{key: "resource_shrimp", rarity: 0, maxAmount: 10, priceLow: 8, priceHigh: 29},
	{key: "resource_crab", rarity: 0, maxAmount: 10, priceLow: 10, priceHigh: 32},
	{key: "resource_coconut", rarity: 0, maxAmount: 5, priceLow: 12, priceHigh: 29},
	{key: "resource_iron_ore", rarity: 0, maxAmount: 10, priceLow: 11, priceHigh: 36},
	{key: "resource_cloth", rarity: 0, maxAmount: 10, priceLow: 6, priceHigh: 20},
	{key: "resource_pearls", rarity: 0, maxAmount: 5, priceLow: 37, priceHigh: 90},
	{key: "resource_black_powder", rarity: 1, maxAmount: 3, priceLow: 318, priceHigh: 747},
	{key: "resource_map_piece_common", rarity: 1, maxAmount: 3, priceLow: 104, priceHigh: 319},
	{key: "resource_map_piece_uncommon", rarity: 1, maxAmount: 2, priceLow: 130, priceHigh: 385},
	{key: "resource_sun_fish", rarity: 1, maxAmount: 3, priceLow: 276, priceHigh: 356},
	{key: "resource_arctic_ice", rarity: 2, maxAmount: 3, priceLow: 1292, priceHigh: 1795},
	{key: "resource_crate", rarity: 2, maxAmount: 5, priceLow: 20, priceHigh: 70},
	{key: "resource_gourmet_treat_basket", rarity: 2, maxAmount: 3, priceLow: 1292, priceHigh: 1795},
	{key: "resource_map_piece_rare", rarity: 2, maxAmount: 1, priceLow: 167, priceHigh: 462},
	{key: "resource_rainbow_fruit", rarity: 2, maxAmount: 3, priceLow: 548, priceHigh: 799},
	{key: "resource_golden_fleece", rarity: 3, maxAmount: 3, priceLow: 1003, priceHigh: 1594},
	{key: "resource_map_piece_ultra", rarity: 3, maxAmount: 3, priceLow: 1286, priceHigh: 3375},
	{key: "resource_root_beer", rarity: 3, maxAmount: 3, priceLow: 820, priceHigh: 1171},
	{key: "resource_twilight_roll", rarity: 3, maxAmount: 3, priceLow: 1125, priceHigh: 1683},
}

func newWorldMarketOffers(now time.Time, rng *rand.Rand, offerCount int) ([]worldMarketOffer, error) {
	if offerCount < 1 || offerCount > len(worldMarketResources) {
		return nil, fmt.Errorf("world market offer count %d is outside 1..%d", offerCount, len(worldMarketResources))
	}
	pools := make(map[int][]worldMarketResource, 4)
	for _, resource := range worldMarketResources {
		pools[resource.rarity] = append(pools[resource.rarity], resource)
	}

	offers := make([]worldMarketOffer, 0, offerCount)
	seen := make(map[string]bool, offerCount)
	for attempts := 0; len(offers) < offerCount && attempts < offerCount*100; attempts++ {
		rarity := weightedWorldMarketRarity(rng)
		pool := pools[rarity]
		resource := pool[rng.Intn(len(pool))]
		if seen[resource.key] {
			continue
		}
		seen[resource.key] = true

		amount := rng.Intn(resource.maxAmount) + 1
		unitPrice := resource.priceLow + rng.Intn(resource.priceHigh-resource.priceLow+1)
		offers = append(offers, worldMarketOffer{
			SaleID:           uuid.New().String(),
			SellerName:       worldMarketSellerNames[rng.Intn(len(worldMarketSellerNames))],
			ItemKey:          resource.key,
			Amount:           amount,
			Price:            amount * unitPrice,
			ExpiryTimeMillis: now.Add(worldMarketOfferLifetime).UnixMilli(),
		})
	}
	if len(offers) != offerCount {
		return nil, fmt.Errorf("generated %d of %d world market offers", len(offers), offerCount)
	}
	return offers, nil
}

var worldMarketSellerNames = []string{"Wesley", "Mara", "Kai", "Nia"}

func weightedWorldMarketRarity(rng *rand.Rand) int {
	roll := rng.Intn(50)
	switch {
	case roll < 22:
		return 0
	case roll < 44:
		return 1
	case roll < 48:
		return 2
	default:
		return 3
	}
}

func worldMarketCachedViewFromOffer(playerUUID string, offer worldMarketOffer, index int, fetchedTimeMillis int64) worldMarketCachedView {
	return worldMarketCachedView{
		BestAlias:     offer.SellerName,
		FeaturedSale:  offer.SaleID,
		FeaturedSlot:  1,
		FetchedTimeMs: fetchedTimeMillis,
		IconURL:       fmt.Sprintf("random player icon %d", index+1),
		Sales: map[string]worldMarketCachedSale{
			offer.SaleID: {
				Amount:        offer.Amount,
				BoughtBy:      []string{},
				Price:         offer.Price,
				PriceResource: "resource_gold",
				Resource:      offer.ItemKey,
				State:         "available",
			},
		},
		Slots: []string{offer.SaleID},
		UUID:  worldMarketSellerUUID(playerUUID, offer.SaleID),
	}
}

func normalizeWorldMarketCacheOffers(playerUUID string, cache *worldMarketCacheFragment, now time.Time) (bool, error) {
	changed := false
	for i, rawView := range cache.View {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawView, &fields); err != nil {
			return false, fmt.Errorf("decode world market cache entry %d: %w", i, err)
		}
		if _, isOffer := fields["saleId"]; !isOffer {
			continue
		}

		var offer worldMarketOffer
		if err := json.Unmarshal(rawView, &offer); err != nil {
			return false, fmt.Errorf("decode legacy world market offer %d: %w", i, err)
		}
		view := worldMarketCachedViewFromOffer(playerUUID, offer, i, now.UnixMilli())
		blob, err := json.Marshal(view)
		if err != nil {
			return false, fmt.Errorf("encode normalized world market view %d: %w", i, err)
		}
		cache.View[i] = blob
		changed = true
	}
	return changed, nil
}

func (s *Store) ensureWorldMarketCache(uuid string) error {
	return s.db.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
		return s.updateWorldMarketCacheInTx(ctx, tx, uuid, false)
	})
}

func (s *Store) ensureWorldMarketCacheInTx(ctx context.Context, tx bun.Tx, playerUUID string) error {
	return s.updateWorldMarketCacheInTx(ctx, tx, playerUUID, false)
}

func (s *Store) refreshWorldMarketCache(playerUUID string) error {
	return s.db.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
		var player Player
		if err := tx.NewSelect().Model(&player).Column("uuid").Where("uuid = ?", playerUUID).Scan(ctx); err != nil {
			return err
		}
		return s.updateWorldMarketCacheInTx(ctx, tx, playerUUID, true)
	})
}

func (s *Store) updateWorldMarketCacheInTx(ctx context.Context, tx bun.Tx, playerUUID string, force bool) error {
	key := worldMarketCacheKey(playerUUID)
	var fragment DocumentFragment
	err := tx.NewSelect().Model(&fragment).Where("key = ?", key).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	now := time.Now().UTC()
	cache := worldMarketCacheFragment{Type: worldMarketCacheType}
	if err == nil {
		if err := json.Unmarshal([]byte(fragment.BlobJSON), &cache); err != nil {
			return fmt.Errorf("decode world market cache for player %s: %w", playerUUID, err)
		}
		if cache.Type != worldMarketCacheType {
			return fmt.Errorf("world market cache for player %s has unexpected type %q", playerUUID, cache.Type)
		}
		if !force && cache.NextRefreshTimeMillis > now.UnixMilli() {
			normalized, err := normalizeWorldMarketCacheOffers(playerUUID, &cache, now)
			if err != nil {
				return fmt.Errorf("normalize world market cache for player %s: %w", playerUUID, err)
			}
			if normalized {
				cache.Token++
				blob, err := json.Marshal(cache)
				if err != nil {
					return fmt.Errorf("encode normalized world market cache for player %s: %w", playerUUID, err)
				}
				return upsertDocumentFragmentInTx(ctx, tx, key, string(blob))
			}
			return nil
		}
	}

	settings, err := getWorldMarketSettingsInTx(ctx, tx)
	if err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(now.UnixNano()))
	offers, err := newWorldMarketOffers(now, rng, settings.OfferCount)
	if err != nil {
		return err
	}
	cache.Token++
	cache.NextRefreshTimeMillis = now.Add(time.Duration(settings.RefreshIntervalMinutes) * time.Minute).UnixMilli()
	cache.View = make([]json.RawMessage, 0, len(offers))
	for i, offer := range offers {
		view := worldMarketCachedViewFromOffer(playerUUID, offer, i, now.UnixMilli())
		blob, err := json.Marshal(view)
		if err != nil {
			return fmt.Errorf("encode world market view: %w", err)
		}
		cache.View = append(cache.View, blob)
	}

	blob, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("encode world market cache for player %s: %w", playerUUID, err)
	}
	hash := sha256.Sum256(blob)
	fragment = DocumentFragment{
		Key:       key,
		BlobJSON:  string(blob),
		Hash:      fmt.Sprintf("%x", hash),
		UpdatedAt: now.Format("2006-01-02 15:04:05"),
	}
	_, err = tx.NewInsert().Model(&fragment).
		On("CONFLICT (key) DO UPDATE SET blob_json = EXCLUDED.blob_json, hash = EXCLUDED.hash, updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (s *Store) ensureWorldMarketCacheForFragment(key string) error {
	const prefix = "GamePlayer:worldMarketCache:P["
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "]") {
		return nil
	}
	playerUUID := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "]")
	if playerUUID == "" {
		return fmt.Errorf("world market fragment key has an empty player UUID")
	}
	return s.ensureWorldMarketCache(playerUUID)
}

func worldMarketCacheKey(playerUUID string) string {
	return "GamePlayer:worldMarketCache:P[" + playerUUID + "]"
}

func worldMarketPlayerUUIDFromDocumentID(documentID string) string {
	if !strings.HasPrefix(documentID, "P[") || !strings.HasSuffix(documentID, "]") {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(documentID, "P["), "]")
}
