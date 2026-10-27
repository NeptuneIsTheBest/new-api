package model

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const pricingDefaultsVersionKey = "PricingDefaultsVersion"

// InitializePricingDefaults materializes the old effective prices exactly once.
// The marker and all maps commit together, before InitOptionMap publishes them.
// Later releases must not bump this marker to seed new defaults: omissions in a
// saved map are deliberate, and only an explicit reset may restore them.
func InitializePricingDefaults() error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if !common.IsMasterNode {
		var marker Option
		if err := DB.Where(&Option{Key: pricingDefaultsVersionKey}).First(&marker).Error; err != nil {
			return fmt.Errorf("pricing defaults must be initialized by the master node before starting this node: %w", err)
		}
		if marker.Value != "1" {
			return errors.New("pricing defaults must be initialized by the master node before starting this node")
		}
		return nil
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		// Creating the anchor also serializes concurrent first starts. Use the
		// common locking helper so SQLite never receives FOR UPDATE.
		marker := Option{Key: pricingDefaultsVersionKey}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where(&Option{Key: pricingDefaultsVersionKey}).First(&marker).Error; err != nil {
			return err
		}
		if marker.Value == "1" {
			return nil
		}
		if marker.Value != "" {
			return fmt.Errorf("unsupported pricing defaults version: %s", marker.Value)
		}

		values, _, duplicated, err := readModelPricingMaps(lockForUpdate(tx))
		if err != nil {
			return err
		}
		if len(duplicated) > 0 {
			return fmt.Errorf("duplicate pricing options: %s", strings.Join(duplicated, ", "))
		}
		for name, expression := range billing_setting.GetBuiltinBillingExprCopy() {
			mode, explicitMode := values["billing_setting.billing_mode"][name]
			if !explicitMode {
				alias := ratio_setting.FormatMatchingModelName(name)
				_, hasRatio := values["ModelRatio"][alias]
				_, hasPrice := values["ModelPrice"][alias]
				if hasRatio || hasPrice {
					continue
				}
				mode = billing_setting.BillingModeTieredExpr
				values["billing_setting.billing_mode"][name] = mode
			}
			if mode == billing_setting.BillingModeTieredExpr {
				if _, configured := values["billing_setting.billing_expr"][name]; !configured {
					values["billing_setting.billing_expr"][name] = expression
				}
			}
		}

		var toolOption Option
		toolJSON := "{}"
		err = lockForUpdate(tx).Where(&Option{Key: operation_setting.ToolPriceOptionKey}).First(&toolOption).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			toolJSON = toolOption.Value
		}
		// Tolerate invalid individual values as the legacy loader did. A malformed
		// map aborts migration instead of overwriting administrator data.
		overrides, err := operation_setting.DecodeToolPricesJSON(toolJSON, true)
		if err != nil {
			return err
		}
		toolPrices := operation_setting.GetDefaultToolPricesCopy()
		maps.Copy(toolPrices, overrides)
		encodedTools, err := common.Marshal(toolPrices)
		if err != nil {
			return err
		}

		rows := make([]Option, 0, len(modelPricingOptionKeys)+1)
		for _, key := range modelPricingOptionKeys {
			encoded, err := common.Marshal(values[key])
			if err != nil {
				return err
			}
			rows = append(rows, Option{Key: key, Value: string(encoded)})
		}
		rows = append(rows, Option{Key: operation_setting.ToolPriceOptionKey, Value: string(encodedTools)})
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).Create(&rows).Error; err != nil {
			return err
		}
		return tx.Model(&marker).Update("value", "1").Error
	})
}
