package nonpbf

import (
	"errors"
	"fmt"
	"go-gin-auth/internal/stock"
	"time"

	"gorm.io/gorm"
)

type IncomingNonPBFService struct {
	db *gorm.DB
}

func NewIncomingNonPBFService(db *gorm.DB) *IncomingNonPBFService {
	return &IncomingNonPBFService{db: db}
}

type IncomingNonPBFServiceInterface interface {
	GetAll(page, limit int) ([]IncomingNonPBF, int64, error)
	GetByID(id uint) (*IncomingNonPBF, error)
	Create(req CreateIncomingNonPBFRequest) (*IncomingNonPBF, error)
	Update(id uint, req UpdateIncomingNonPBFRequest) (*IncomingNonPBF, error)
	Delete(id uint) error
}

func (s *IncomingNonPBFService) GetAll(page, limit int) ([]IncomingNonPBF, int64, error) {
	var incomings []IncomingNonPBF
	var total int64

	offset := (page - 1) * limit

	if err := s.db.Model(IncomingNonPBF{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := s.db.Preload("User").Preload("IncomingNonPBFDetails.Product").
		Offset(offset).Limit(limit).Order("created_at DESC").Find(&incomings).Error

	return incomings, total, err
}

func (s *IncomingNonPBFService) GetByID(id uint) (*IncomingNonPBF, error) {
	var incoming IncomingNonPBF

	err := s.db.Preload("User").Preload("IncomingNonPBFDetails.Product").
		First(&incoming, id).Error

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("data tidak ditemukan")
		}
		return nil, err
	}

	return &incoming, nil
}

func (s *IncomingNonPBFService) Create(req CreateIncomingNonPBFRequest) (*IncomingNonPBF, error) {
	tx := s.db.Begin()

	// Generate transaction code
	transactionCode := s.generateTransactionCode()

	// Calculate total purchase
	var totalPurchase float64
	for _, detail := range req.Details {
		totalPurchase += detail.PurchasePrice * float64(detail.IncomingQuantity)
	}

	// Set default payment status
	paymentStatus := req.PaymentStatus
	if paymentStatus == "" {
		paymentStatus = "Belum Lunas"
	}

	incoming := IncomingNonPBF{
		OrderNumber:     req.OrderNumber,
		OrderDate:       req.OrderDate,
		IncomingDate:    req.IncomingDate,
		TransactionCode: transactionCode,
		SupplierName:    req.SupplierName,
		InvoiceNumber:   req.InvoiceNumber,
		TransactionType: req.TransactionType,
		PaymentDueDate:  req.PaymentDueDate,
		OfficerName:     req.OfficerName,
		AdditionalNotes: req.AdditionalNotes,
		TotalPurchase:   totalPurchase,
		PaymentStatus:   paymentStatus,
		UserID:          req.UserID,
	}

	if err := tx.Create(&incoming).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	// Create details
	for _, detailReq := range req.Details {
		detail := IncomingNonPBFDetail{
			IncomingNonPBFID: incoming.ID,
			ProductCode:      detailReq.ProductCode,
			ProductName:      detailReq.ProductName,
			Unit:             detailReq.Unit,
			IncomingQuantity: detailReq.IncomingQuantity,
			PurchasePrice:    detailReq.PurchasePrice,
			TotalPurchase:    detailReq.PurchasePrice * float64(detailReq.IncomingQuantity),
			BatchNumber:      detailReq.BatchNumber,
			ExpiryDate:       detailReq.ExpiryDate,
			ProductID:        detailReq.ProductID,
		}

		if err := tx.Create(&detail).Error; err != nil {
			tx.Rollback()
			return nil, err
		}

		// **UPDATE STOCK - TAMBAH STOK MASUK**
		if err := s.updateStock(tx, detailReq, "ADD"); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update stock: %v", err)
		}
	}

	tx.Commit()

	// Reload with relations
	return s.GetByID(incoming.ID)
}

func (s *IncomingNonPBFService) Update(id uint, req UpdateIncomingNonPBFRequest) (*IncomingNonPBF, error) {
	tx := s.db.Begin()

	var incoming IncomingNonPBF
	if err := tx.First(&incoming, id).Error; err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("data tidak ditemukan")
		}
		return nil, err
	}

	// **GET OLD DETAILS FOR STOCK REVERSAL**
	var oldDetails []IncomingNonPBFDetail
	if err := tx.Where("incoming_non_pbf_id = ?", id).Find(&oldDetails).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	// **REVERT OLD STOCK CHANGES**
	for _, oldDetail := range oldDetails {
		detailReq := CreateIncomingDetailRequest{
			ProductID:        oldDetail.ProductID,
			ProductCode:      oldDetail.ProductCode,
			ProductName:      oldDetail.ProductName,
			Unit:             oldDetail.Unit,
			IncomingQuantity: oldDetail.IncomingQuantity,
			BatchNumber:      oldDetail.BatchNumber,
			ExpiryDate:       oldDetail.ExpiryDate,
		}
		if err := s.updateStock(tx, detailReq, "SUBTRACT"); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to revert stock: %v", err)
		}
	}
	// Calculate total purchase
	var totalPurchase float64
	for _, detail := range req.Details {
		totalPurchase += detail.PurchasePrice * float64(detail.IncomingQuantity)
	}

	// Update main record
	updates := map[string]interface{}{
		"total_purchase": totalPurchase,
	}

	if req.OrderNumber != "" {
		updates["order_number"] = req.OrderNumber
	}
	if !req.OrderDate.IsZero() {
		updates["order_date"] = req.OrderDate
	}
	if !req.IncomingDate.IsZero() {
		updates["incoming_date"] = req.IncomingDate
	}
	if req.SupplierName != "" {
		updates["supplier_name"] = req.SupplierName
	}
	if req.InvoiceNumber != "" {
		updates["invoice_number"] = req.InvoiceNumber
	}
	if req.TransactionType != "" {
		updates["transaction_type"] = req.TransactionType
	}
	if req.PaymentDueDate != nil {
		updates["payment_due_date"] = req.PaymentDueDate
	}
	if req.OfficerName != "" {
		updates["officer_name"] = req.OfficerName
	}
	if req.AdditionalNotes != "" {
		updates["additional_notes"] = req.AdditionalNotes
	}
	if req.PaymentStatus != "" {
		updates["payment_status"] = req.PaymentStatus
	}

	if err := tx.Model(&incoming).Updates(updates).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	// Delete existing details
	if err := tx.Where("incoming_non_pbf_id = ?", id).Delete(&IncomingNonPBFDetail{}).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	// Create new details
	for _, detailReq := range req.Details {
		detail := IncomingNonPBFDetail{
			IncomingNonPBFID: incoming.ID,
			ProductCode:      detailReq.ProductCode,
			ProductName:      detailReq.ProductName,
			Unit:             detailReq.Unit,
			IncomingQuantity: detailReq.IncomingQuantity,
			PurchasePrice:    detailReq.PurchasePrice,
			TotalPurchase:    detailReq.PurchasePrice * float64(detailReq.IncomingQuantity),
			BatchNumber:      detailReq.BatchNumber,
			ExpiryDate:       detailReq.ExpiryDate,
			ProductID:        detailReq.ProductID,
		}

		if err := tx.Create(&detail).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		// **UPDATE STOCK - TAMBAH STOK BARU**
		if err := s.updateStock(tx, detailReq, "ADD"); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update stock: %v", err)
		}
	}

	tx.Commit()

	// Reload with relations
	return s.GetByID(incoming.ID)
}

func (s *IncomingNonPBFService) Delete(id uint) error {
	tx := s.db.Begin()

	var incoming IncomingNonPBF
	if err := tx.First(&incoming, id).Error; err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("data tidak ditemukan")
		}
		return err
	}
	// **GET DETAILS FOR STOCK REVERSAL**
	var details []IncomingNonPBFDetail
	if err := tx.Where("incoming_non_pbf_id = ?", id).Find(&details).Error; err != nil {
		tx.Rollback()
		return err
	}
	// **REVERT STOCK CHANGES**
	for _, detail := range details {
		detailReq := CreateIncomingDetailRequest{
			ProductID:        detail.ProductID,
			ProductCode:      detail.ProductCode,
			ProductName:      detail.ProductName,
			Unit:             detail.Unit,
			IncomingQuantity: detail.IncomingQuantity,
			BatchNumber:      detail.BatchNumber,
			ExpiryDate:       detail.ExpiryDate,
		}
		if err := s.updateStock(tx, detailReq, "SUBTRACT"); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to revert stock: %v", err)
		}
	}
	// Delete details first
	if err := tx.Where("incoming_non_pbf_id = ?", id).Delete(&IncomingNonPBFDetail{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	// Delete main record
	if err := tx.Delete(&incoming).Error; err != nil {
		tx.Rollback()
		return err
	}

	tx.Commit()
	return nil
}

// **FUNGSI UTAMA UNTUK UPDATE STOCK**
func (s *IncomingNonPBFService) updateStock(tx *gorm.DB, detail CreateIncomingDetailRequest, operation string) error {
	var stockdata stock.Stock

	// Cari stok berdasarkan ProductID
	err := tx.Where("product_id = ?", detail.ProductID).
		First(&stockdata).Error

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// **BUAT STOCK BARU JIKA BELUM ADA**
			if operation == "ADD" {
				newStock := stock.Stock{
					ProductID:    *detail.ProductID,
					Quantity:     detail.IncomingQuantity,
					ExpiryDate:   detail.ExpiryDate,
					MinimumStock: 10, // Atur sesuai kebutuhan
				}
				return tx.Create(&newStock).Error
			}
			// Jika operation SUBTRACT tapi stock tidak ada, return error
			return fmt.Errorf("stock not found for product %s batch %s", detail.ProductCode, detail.BatchNumber)
		}
		return err
	}

	// **UPDATE STOCK YANG SUDAH ADA**
	var newQuantity int
	switch operation {
	case "ADD":
		newQuantity = stockdata.Quantity + detail.IncomingQuantity
	case "SUBTRACT":
		newQuantity = stockdata.Quantity - detail.IncomingQuantity
		if newQuantity < 0 {
			return fmt.Errorf("insufficient stock for product %s batch %s", detail.ProductCode, detail.BatchNumber)
		}
	}

	// ✅ Update quantity dan expired-nya
	return tx.Model(&stockdata).Updates(map[string]interface{}{
		"quantity":    newQuantity,
		"expiry_date": detail.ExpiryDate,
	}).Error
}
func (s *IncomingNonPBFService) generateTransactionCode() string {
	now := time.Now()
	return fmt.Sprintf("NONPBF-%s-%d", now.Format("20060102"), now.Unix())
}
