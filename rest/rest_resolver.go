package rest

import (
	"bytes"
	"context"
	"crypto/sha512"
	"database/sql"
	"fmt"
	"image"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/maddsua/flippercardapp/auth"
	db_pkg "github.com/maddsua/flippercardapp/db"
	db_gen "github.com/maddsua/flippercardapp/db/generated"
	db_model "github.com/maddsua/flippercardapp/db/model"
	db_types "github.com/maddsua/flippercardapp/db/types"
	"github.com/maddsua/flippercardapp/rest/model"

	"github.com/maddsua/flippercardapp/utils"
	libwebp "github.com/pixiv/go-libwebp/webp"
	"golang.org/x/image/draw"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

type resolver struct {
	db *db_pkg.Wrapper
}

func (rslv *resolver) LoadCardDeck(ctx context.Context, deckID uuid.UUID) (*model.CardDeck, error) {

	deck, err := rslv.db.GetDeckById(ctx, deckID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "deck not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetDeckById", err)
	} else if err := EnforceResourceVisibility(ctx, deck.Visibility); err != nil {
		return nil, err
	}

	result := model.CardDeck{
		CardDeckMeta: db_pkg.TransformRow[model.CardDeckMeta](deck),
		Labels:       []string{deck.Name},
		Cards:        make([]db_model.CardNode, 0),
	}

	if version, err := rslv.getDeckLatestVersion(ctx, &rslv.db.Queries, deck.ID, deck.LatestVersionID); err != nil {
		return nil, err
	} else if version.Valid {
		result.VersionID = db_types.NewNullUUID(version.V.ID)
		result.Cards = version.V.Content.Cards
		result.Size = int(version.V.CardCount)
	}

	if collection, err := rslv.db.GetCollectionById(ctx, deck.CollectionID); err != nil {
		return nil, InternalError("sqlc.GetCollectionById", err)
	} else {
		result.Labels = append(result.Labels, collection.Name)
	}

	return &result, nil
}

func (rslv *resolver) getDeckLatestVersion(ctx context.Context, tx *db_gen.Queries, deckID uuid.UUID, latestVersionID uuid.NullUUID) (sql.Null[db_gen.DeckVersion], error) {

	if latestVersionID.Valid {

		if version, err := tx.GetDeckVersion(ctx, db_gen.GetDeckVersionParams{VersionID: latestVersionID.UUID}); err != nil && !db_pkg.IsNull(err) {
			return sql.Null[db_gen.DeckVersion]{}, InternalError("sqlc.GetDeckVersion", err)
		} else if err == nil {
			return sql.Null[db_gen.DeckVersion]{V: version, Valid: true}, nil
		}

		slog.Warn("REST DATA: Deck latest version wasn't found by it's referenced ID",
			slog.String("deck_id", deckID.String()))

		return sql.Null[db_gen.DeckVersion]{}, nil
	}

	version, err := tx.GetDeckLatestVersion(ctx, deckID)
	if err != nil && !db_pkg.IsNull(err) {
		return sql.Null[db_gen.DeckVersion]{}, InternalError("sqlc.GetDeckLatestVersion", err)
	}

	return sql.Null[db_gen.DeckVersion]{V: version, Valid: true}, nil
}

func (rslv *resolver) ListCardDeckVersions(ctx context.Context, deckID uuid.UUID, page PagePointers) (*Page[model.CardDeckVersionMeta], error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsTeamMember(); err != nil {
		return nil, err
	}

	deck, err := rslv.db.GetDeckById(ctx, deckID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "deck not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetDeckById", err)
	}

	entries, err := rslv.db.GetDeckVersionsBatch(ctx, db_gen.GetDeckVersionsBatchParams{
		DeckID: deck.ID,
		Limit:  page.QueryLimit(),
		Offset: page.QueryOffset(),
	})
	if err != nil {
		return nil, InternalError("sqlc.GetDeckVersionsBatch", err)
	}

	result := TransformPage(page, entries, db_pkg.TransformRow[model.CardDeckVersionMeta, db_gen.DeckVersion])

	if deck.LatestVersionID.Valid {
		for idx, val := range result.Entries {
			if val.ID == deck.LatestVersionID.UUID {
				result.Entries[idx].IsLatest = true
				break
			}
		}
	}

	return result, nil
}
func (rslv *resolver) DeleteCardDeckVersion(ctx context.Context, deckID uuid.UUID, versionID uuid.UUID) error {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return err
	} else if err := perms.AsContentEditor(); err != nil {
		return err
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	version, err := tx.GetDeckVersion(ctx, db_gen.GetDeckVersionParams{
		DeckID:    db_types.NewNullUUID(deckID),
		VersionID: versionID,
	})

	if db_pkg.IsNull(err) {
		return &model.Error{Message: "Version not found", Code: http.StatusNotFound}
	} else if err != nil {
		return InternalError("sqlc.GetDeckVersion", err)
	}

	if deck, err := tx.GetDeckById(ctx, version.DeckID); err != nil {
		return InternalError("sqlc.GetDeckById", err)
	} else if deck.LatestVersionID.UUID == version.ID {
		return &model.Error{Message: "Unable to delete the latest deck version", Code: http.StatusBadRequest}
	}

	if _, err := tx.DeleteDeckVersion(ctx, db_gen.DeleteDeckVersionParams{
		VersionID: version.ID,
		DeckID:    version.DeckID,
	}); err != nil {
		return InternalError("sqlc.DeleteDeckVersion", err)
	}

	if err := tx.Commit(); err != nil {
		return InternalError("sqlc.Commit", err)
	}

	return nil
}

func (rslv *resolver) LoadCardDeckVersion(ctx context.Context, deckID uuid.UUID, versionID uuid.UUID) (*model.CardDeckVersion, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsTeamMember(); err != nil {
		return nil, err
	}

	entry, err := rslv.db.GetDeckVersion(ctx, db_gen.GetDeckVersionParams{DeckID: db_types.NewNullUUID(deckID), VersionID: versionID})
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "Version not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetDeckVersion", err)
	}

	result := db_pkg.TransformRow[model.CardDeckVersion](entry)
	return &result, nil
}

func (rslv *resolver) ListCardDeckBatch(ctx context.Context, ids uuid.UUIDs, page PagePointers) (*Page[model.CardDeckMeta], error) {

	entries, err := rslv.db.GetDecksBatch(ctx, db_gen.GetDecksBatchParams{
		IdsSet:        db_types.NewNullUUIDs(ids),
		VisibilitySet: ResourceVisibilityFilter(ctx, ids),
		Limit:         page.QueryLimit(),
		Offset:        page.QueryOffset(),
	})

	if err != nil {
		return nil, InternalError("sqlc.GetDecksBatch", err)
	}

	return TransformPage(page, entries, db_pkg.TransformBatchRow[model.CardDeckMeta, db_gen.GetDecksBatchRow]), nil
}

func (rslv *resolver) LoadCollection(ctx context.Context, collectionID uuid.UUID) (*model.Collection, error) {

	collection, err := rslv.db.GetCollectionById(ctx, collectionID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "collection not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetCollectionById", err)
	} else if err := EnforceResourceVisibility(ctx, collection.Visibility); err != nil {
		return nil, err
	}

	decks, err := rslv.db.GetDecksBatch(ctx, db_gen.GetDecksBatchParams{
		CollectionID:  db_types.NewNullUUID(collection.ID),
		VisibilitySet: ResourceVisibilityFilter(ctx, nil),
		Limit:         math.MaxInt32,
	})

	if err != nil {
		return nil, InternalError("sqlc.GetDecksBatch", err)
	}

	result := model.Collection{
		CollectionMeta: db_pkg.TransformRow[model.CollectionMeta](collection),
		Decks:          make([]model.CardDeckMeta, len(decks)),
	}

	result.CollectionMeta.Size = len(decks)

	for idx, val := range decks {
		result.Decks[idx].FromBatchRow(val)
	}

	return &result, nil
}

func (rslv *resolver) ListCollectionsBatch(ctx context.Context, ids uuid.UUIDs, page PagePointers) (*Page[model.CollectionMeta], error) {

	entries, err := rslv.db.GetCollectionBatch(ctx, db_gen.GetCollectionBatchParams{
		IdsSet:        db_types.NewNullUUIDs(ids),
		VisibilitySet: ResourceVisibilityFilter(ctx, ids),
		Limit:         page.QueryLimit(),
		Offset:        page.QueryOffset(),
	})
	if err != nil {
		return nil, InternalError("sqlc.GetCollectionBatch", err)
	}

	return TransformPage(page, entries, db_pkg.TransformBatchRow[model.CollectionMeta, db_gen.GetCollectionBatchRow]), nil
}

func (rslv *resolver) SearchCollections(ctx context.Context, term string, page PagePointers) (*Page[model.CollectionSearchResult], error) {

	matchIndex, err := rslv.fuzzyIndexCollection(ctx, term)
	if err != nil {
		return nil, err
	} else if len(matchIndex) == 0 {
		return WrapPage(page, []model.CollectionSearchResult{}), nil
	}

	entries, err := rslv.db.GetCollectionBatch(ctx, db_gen.GetCollectionBatchParams{
		IdsSet: db_types.NewNullUUIDs(UnwrapSearchIndex(matchIndex, page)),
		Limit:  page.QueryLimit(),
	})

	if err != nil {
		return nil, InternalError("sqlc.GetCollectionBatch", err)
	}

	result := TransformPage(page, entries, func(row db_gen.GetCollectionBatchRow) model.CollectionSearchResult {
		var next model.CollectionSearchResult
		next.FromBatchRow(row)
		next.Rank = matchIndex[row.ID]
		return next
	})

	sort.SliceStable(result.Entries, func(i, j int) bool {
		return result.Entries[i].Rank < result.Entries[j].Rank
	})

	return result, nil
}

func (rslv *resolver) fuzzyIndexCollection(ctx context.Context, term string) (map[uuid.UUID]int, error) {

	if term = strings.ToLower(strings.TrimSpace(term)); len(term) < 2 {
		return nil, &model.Error{Message: "Search term too short"}
	} else if len(term) > math.MaxUint8 {
		return nil, &model.Error{Message: "Search term too long", Code: http.StatusRequestEntityTooLarge}
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return nil, InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	const indexBatchSize = 100

	matchIndex := map[uuid.UUID]int{}

	for offset := 0; offset < math.MaxInt; offset += indexBatchSize {

		next, err := tx.GetCollectionSearchBatch(ctx, db_gen.GetCollectionSearchBatchParams{
			VisibilitySet: ResourceVisibilityFilter(ctx, nil),
			Offset:        int64(offset),
			Limit:         indexBatchSize,
		})

		if err != nil {
			return nil, InternalError("sqlc.GetCollectionSearchBatch", err)
		} else if len(next) == 0 {
			break
		}

		for _, item := range next {
			doc := strings.ToLower(item.Name)
			if rank := fuzzy.RankMatch(term, doc); rank >= 0 {
				matchIndex[item.ID] = rank
			}
		}
	}

	return matchIndex, nil
}

func (rslv *resolver) CreateContentCollection(ctx context.Context, params model.CollectionPatch) (*model.CollectionMeta, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsContentEditor(); err != nil {
		return nil, err
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return nil, InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	if err := params.Valid(); err != nil {
		return nil, &model.Error{Message: fmt.Sprintf("Invalid collection details: %v", err)}
	}

	if exists, err := tx.CollectionNameExists(ctx, params.Name); err != nil {
		return nil, InternalError("sqlc.CollectionNameExists", err)
	} else if exists {
		return nil, &model.Error{
			Message: "Collection name already exists",
			Code:    http.StatusConflict,
		}
	}

	entry, err := tx.InsertCollection(ctx, db_gen.InsertCollectionParams{
		ID:          uuid.New(),
		CreatedAt:   db_types.NewTime(time.Now()),
		UpdatedAt:   db_types.NewTime(time.Now()),
		Name:        params.Name,
		Description: db_types.NewNullString(params.Description),
		Visibility:  params.Visibility,
	})

	if err != nil {
		return nil, InternalError("sqlc.InsertCollection", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, InternalError("sqlc.Commit", err)
	}

	result := db_pkg.TransformRow[model.CollectionMeta](entry)
	return &result, nil
}

func (rslv *resolver) UpdateContentCollection(ctx context.Context, collectionID uuid.UUID, params model.CollectionPatch) (*model.CollectionMeta, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsContentEditor(); err != nil {
		return nil, err
	}

	if err := params.Valid(); err != nil {
		return nil, &model.Error{Message: fmt.Sprintf("Invalid collection details: %v", err)}
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return nil, InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	entry, err := tx.GetCollectionById(ctx, collectionID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "Collection not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetCollectionById", err)
	}

	if entry.Visibility != params.Visibility {
		if _, err = tx.UpdateCollectionChildrenVisibility(ctx, db_gen.UpdateCollectionChildrenVisibilityParams{
			CollectionID:  entry.ID,
			OldVisibility: entry.Visibility,
			NewVisibility: params.Visibility,
		}); err != nil {
			return nil, InternalError("sqlc.UpdateCollectionChildrenVisibility", err)
		}
	}

	if entry, err = tx.UpdateCollection(ctx, db_gen.UpdateCollectionParams{
		ID:          collectionID,
		Name:        params.Name,
		Description: db_types.NewNullString(params.Description),
		Visibility:  params.Visibility,
		UpdatedAt:   db_types.NewTime(time.Now()),
		ThemeColor:  db_types.NewNullString(params.ThemeColor),
	}); err != nil {
		return nil, InternalError("sqlc.UpdateCollection", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, InternalError("sqlc.Commit", err)
	}

	result := db_pkg.TransformRow[model.CollectionMeta](entry)
	return &result, nil
}

func (rslv *resolver) DeleteCollection(ctx context.Context, collectionID uuid.UUID, recursive bool) error {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return err
	} else if err := perms.AsContentEditor(); err != nil {
		return err
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	if !recursive {

		if count, err := tx.CollectionSize(ctx, collectionID); err != nil {

			if db_pkg.IsNull(err) {
				return &model.Error{Message: "Collection not found", Code: http.StatusNotFound}
			}

			return InternalError("sqlc.CollectionSize", err)

		} else if count > 0 {
			return &model.Error{Message: "Collection is not empty", Code: http.StatusConflict}
		}
	}

	if count, err := tx.DeleteCollection(ctx, collectionID); err != nil {
		return InternalError("sqlc.DeleteCollection", err)
	} else if count == 0 {
		return &model.Error{Message: "Collectcion not found", Code: http.StatusNotFound}
	}

	if err := tx.Commit(); err != nil {
		return InternalError("sqlc.Commit", err)
	}

	return nil
}

func (rslv *resolver) CreateCardDeck(ctx context.Context, params model.CardDeckPatch) (*model.CardDeckMeta, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsContentEditor(); err != nil {
		return nil, err
	}

	if params.Summary == nil {
		return nil, &model.Error{Message: "Deck details must be provided"}
	} else if err := params.Summary.Valid(); err != nil {
		return nil, &model.Error{Message: fmt.Sprintf("Invalid deck summary: %v", err)}
	} else if params.Content == nil || len(params.Content.Cards) == 0 {
		return nil, &model.Error{Message: "Deck has no cards in it"}
	} else if !params.CollectionID.Valid {
		return nil, &model.Error{Message: "Collection ID must be provided"}
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return nil, InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	if count, err := tx.DeckNameExists(ctx, db_gen.DeckNameExistsParams{Name: params.Summary.Name}); err != nil {
		return nil, InternalError("sqlc.DeckNameExists", err)
	} else if count != 0 {
		return nil, &model.Error{Message: "A deck with the same name already exists"}
	}

	deck, err := tx.InsertDeck(ctx, db_gen.InsertDeckParams{
		ID:           uuid.New(),
		CollectionID: params.CollectionID.UUID,
		CreatedAt:    db_types.NewTime(time.Now()),
		UpdatedAt:    db_types.NewTime(time.Now()),
		Name:         params.Summary.Name,
		Description:  db_types.NewNullString(params.Summary.Description),
		Visibility:   db_model.ResourceVisibilityFromPtr(params.Visibility),
	})

	if err != nil {
		return nil, InternalError("sqlc.InsertDeck", err)
	}

	if affected, err := tx.UpdateCollectionContentMtime(ctx, deck.CollectionID); err != nil {
		return nil, InternalError("sqlc.UpdateCollectionMtime", err)
	} else if affected != 1 {
		return nil, &model.Error{Message: "Collection ID not found", Code: http.StatusNotFound}
	}

	result := db_pkg.TransformRow[model.CardDeckMeta](deck)

	version, err := rslv.addDeckVersion(ctx, &tx.Queries, deck.ID, "Deck created", db_model.CardDeckVersionContent{
		Summary: *params.Summary,
		Cards:   params.Content.Cards,
	})

	if err != nil {
		return nil, err
	}

	result.VersionID = db_types.NewNullUUID(version.ID)
	result.Size = len(version.Content.Cards)

	if err := tx.Commit(); err != nil {
		return nil, InternalError("sqlc.Commit", err)
	}

	return &result, nil
}

func (rslv *resolver) addDeckVersion(ctx context.Context, tx *db_gen.Queries, deckID uuid.UUID, label string, content db_model.CardDeckVersionContent) (db_gen.DeckVersion, error) {

	entry, err := tx.InsertDeckVersion(ctx, db_gen.InsertDeckVersionParams{
		ID:        uuid.New(),
		CreatedAt: db_types.NewTime(time.Now()),
		DeckID:    deckID,
		CardCount: int64(len(content.Cards)),
		Content:   content,
		Label:     db_types.NewNullString(label),
	})
	if err != nil {
		return db_gen.DeckVersion{}, InternalError("sqlc.InsertDeckVersion", err)
	}

	if _, err = tx.SetDeckLatestVersion(ctx, db_gen.SetDeckLatestVersionParams{
		DeckID:          entry.DeckID,
		UpdatedAt:       db_types.NewNullTime(time.Now()),
		LatestVersionID: db_types.NewNullUUID(entry.ID),
	}); err != nil {
		return db_gen.DeckVersion{}, InternalError("sqlc.SetDeckLatestVersion", err)
	}

	return entry, nil
}

func (rslv *resolver) UpdateCardDeck(ctx context.Context, deckID uuid.UUID, params model.CardDeckPatch) (*model.CardDeckMeta, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsContentEditor(); err != nil {
		return nil, err
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return nil, InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	deck, err := tx.GetDeckById(ctx, deckID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "Deck ID not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetDeckById", err)
	}

	metaPatchParams := sql.Null[db_gen.UpdateDeckMetadataParams]{
		V: db_gen.UpdateDeckMetadataParams{
			ID:           deckID,
			CollectionID: deck.CollectionID,
			UpdatedAt:    db_types.NewTime(time.Now()),
			Name:         deck.Name,
			Description:  deck.Description,
			Visibility:   deck.Visibility,
		},
	}

	//	note: this moves this deck to a specified collection and updates it's mtime
	if params.CollectionID.Valid {

		metaPatchParams.Valid = true
		metaPatchParams.V.CollectionID = params.CollectionID.UUID

		if affected, err := tx.UpdateCollectionContentMtime(ctx, params.CollectionID.UUID); err != nil {
			return nil, InternalError("sqlc.UpdateCollectionMtime", err)
		} else if affected != 1 {
			return nil, &model.Error{Message: "Collection ID not found", Code: http.StatusNotFound}
		}
	}

	contentVersionParams := sql.Null[db_model.CardDeckVersionContent]{
		V: db_model.CardDeckVersionContent{
			Summary: db_model.ContentSummary{
				Name:        deck.Name,
				Description: deck.Description.String,
			},
		},
	}

	if params.Summary != nil {

		if err := params.Summary.Valid(); err != nil {
			return nil, &model.Error{Message: fmt.Sprintf("Invalid deck summary: %v", err)}
		}

		if count, err := tx.DeckNameExists(ctx, db_gen.DeckNameExistsParams{
			Name:   params.Summary.Name,
			DeckID: db_types.NewNullUUID(deckID),
		}); err != nil {
			return nil, InternalError("sqlc.DeckNameExists", err)
		} else if count != 0 {
			return nil, &model.Error{Message: "A deck with the same name already exists"}
		}

		metaPatchParams.Valid = true
		metaPatchParams.V.Name = params.Summary.Name
		metaPatchParams.V.Description = db_types.NewNullString(params.Summary.Description)

		contentVersionParams.V.Summary = db_model.ContentSummary{
			Name:        params.Summary.Name,
			Description: params.Summary.Description,
		}
	}

	if params.Visibility != nil {
		metaPatchParams.Valid = true
		metaPatchParams.V.Visibility = db_model.ResourceVisibilityFromPtr(params.Visibility)
	}

	if params.Content != nil {

		if len(params.Content.Cards) == 0 {
			return nil, &model.Error{Message: "Deck has no cards in it"}
		}

		contentVersionParams.Valid = true
		contentVersionParams.V.Cards = params.Content.Cards

	} else if params.Summary != nil || params.Label != "" {

		if version, err := rslv.getDeckLatestVersion(ctx, &tx.Queries, deck.ID, deck.LatestVersionID); err != nil {
			return nil, err
		} else if version.Valid {
			contentVersionParams.V = version.V.Content
			contentVersionParams.Valid = true
		}
	}

	if metaPatchParams.Valid {
		if deck, err = tx.UpdateDeckMetadata(ctx, metaPatchParams.V); err != nil {
			return nil, InternalError("sqlc.UpdateDeckMetadata", err)
		}
	}

	result := db_pkg.TransformRow[model.CardDeckMeta](deck)

	if contentVersionParams.Valid {

		version, err := rslv.addDeckVersion(ctx, &tx.Queries, deck.ID, params.Label, contentVersionParams.V)
		if err != nil {
			return nil, err
		}

		result.VersionID = db_types.NewNullUUID(version.ID)
		result.Size = len(version.Content.Cards)
	}

	if err := tx.Commit(); err != nil {
		return nil, InternalError("sqlc.Commit", err)
	}

	return &result, nil
}

func (rslv *resolver) DeleteDeck(ctx context.Context, deckID uuid.UUID) error {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return err
	} else if err := perms.AsContentEditor(); err != nil {
		return err
	}

	tx, err := rslv.db.BeginTx(ctx)
	if err != nil {
		return InternalError("sqlc.BeginTx", err)
	}
	defer tx.Rollback()

	entry, err := tx.DeleteDeck(ctx, deckID)
	if db_pkg.IsNull(err) {
		return &model.Error{Message: "Deck not found", Code: http.StatusNotFound}
	} else if err != nil {
		return InternalError("sqlc.DeleteDeck", err)
	}

	if affected, err := tx.UpdateCollectionContentMtime(ctx, entry.CollectionID); err != nil {
		return InternalError("sqlc.UpdateCollectionMtime", err)
	} else if affected != 1 {
		return &model.Error{Message: "Collection ID not found", Code: http.StatusNotFound}
	}

	if err := tx.Commit(); err != nil {
		return InternalError("sqlc.Commit", err)
	}

	return nil
}

func (rslv *resolver) UploadImage(ctx context.Context, name string, reader io.Reader) (*model.ImageMeta, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsContentEditor(); err != nil {
		return nil, err
	}

	const maxCanvasSize = 1440

	sourceHash := sha512.New()
	source, _, err := image.Decode(io.TeeReader(reader, sourceHash))
	if err != nil {
		return nil, &model.Error{Message: "Unable to decode image: " + err.Error(), Code: http.StatusUnprocessableEntity}
	}

	if entry, err := rslv.db.GetImageByHash(ctx, sourceHash.Sum(nil)); err == nil {
		var result model.ImageMeta
		result.FromRow(entry)
		return &result, nil
	} else if !db_pkg.IsNull(err) {
		return nil, InternalError("sqlc.GetImageByHash", err)
	}

	sourceSize := source.Bounds().Size()
	if min(sourceSize.X, sourceSize.Y) < 100 || max(sourceSize.X, sourceSize.Y) > 10_000 {
		return nil, &model.Error{Message: "Invalid image dimensions", Code: http.StatusUnprocessableEntity}
	}

	if sourceAspect := float64(sourceSize.X) / float64(sourceSize.Y); sourceAspect > 3 || sourceAspect < 0.65 {
		return nil, &model.Error{Message: "Invalid image aspect ratio", Code: http.StatusUnprocessableEntity}
	}

	targetBounds := image.Rect(0, 0, sourceSize.X, sourceSize.Y)

	if size := targetBounds.Size(); size.X > maxCanvasSize {
		targetBounds = image.Rect(0, 0, maxCanvasSize, int(float64(size.Y)*(float64(maxCanvasSize)/float64(size.X))))
	}

	if size := targetBounds.Size(); size.Y > maxCanvasSize {
		targetBounds = image.Rect(0, 0, int(float64(size.X)*(float64(maxCanvasSize)/float64(size.Y))), maxCanvasSize)
	}

	canvas := image.NewRGBA(targetBounds)
	draw.CatmullRom.Scale(canvas, canvas.Rect, source, source.Bounds(), draw.Over, nil)

	var buffer bytes.Buffer

	encoderConfig, err := libwebp.ConfigPreset(libwebp.PresetDefault, 90)
	if err != nil {
		return nil, InternalError("webp.ConfigPreset", err)
	}

	if err := libwebp.EncodeRGBA(&buffer, canvas, encoderConfig); err != nil {
		return nil, InternalError("webp.EncodeRGBA", err)
	}

	dataHash := sha512.New()
	data, err := io.ReadAll(io.TeeReader(&buffer, dataHash))
	if err != nil {
		return nil, InternalError("io.TeeReader -> sha512", err)
	}

	if name == "" {
		name = "Image upload"
	}

	entry, err := rslv.db.InsertImage(ctx, db_gen.InsertImageParams{
		ID:               utils.NewRandomBase64Token(64),
		CreatedAt:        db_types.NewTime(time.Now()),
		Mimetype:         "image/webp",
		SourceName:       name,
		SourceSha512Hash: sourceHash.Sum(nil),
		DataSha512Hash:   dataHash.Sum(nil),
		DataSize:         int64(len(data)),
		Data:             data,
	})

	if err != nil {
		return nil, InternalError("sqlc.InsertImage", err)
	}

	var result model.ImageMeta
	result.FromRow(entry)
	return &result, nil
}

func (rslv *resolver) ImageMetadata(ctx context.Context, mediaID string) (*model.ImageMeta, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsTeamMember(); err != nil {
		return nil, err
	}

	entry, err := rslv.db.GetImageById(ctx, mediaID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "image not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetImageById", err)
	}

	var result model.ImageMeta
	result.FromRow(entry)
	return &result, nil
}

func (rslv *resolver) ImageBlob(ctx context.Context, mediaID string) (io.Reader, error) {

	if perms, err := auth.For(ctx).Permissions(); err != nil {
		return nil, err
	} else if err := perms.AsTeamMember(); err != nil {
		return nil, err
	}

	entry, err := rslv.db.GetImageById(ctx, mediaID)
	if db_pkg.IsNull(err) {
		return nil, &model.Error{Message: "image not found", Code: http.StatusNotFound}
	} else if err != nil {
		return nil, InternalError("sqlc.GetImageById", err)
	}

	return bytes.NewReader(entry.Data), nil
}
