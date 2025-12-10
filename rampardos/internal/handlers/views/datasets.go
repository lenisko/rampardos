package views

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/services"
)

// DatasetsView renders dataset pages
type DatasetsView struct {
	datasetsController *services.DatasetsController
	openFreeMapService *services.OpenFreeMapService
	downloadManager    *services.DownloadManager
	templates          *TemplateRenderer
}

// NewDatasetsView creates a new datasets view
func NewDatasetsView(datasetsController *services.DatasetsController, openFreeMapService *services.OpenFreeMapService, downloadManager *services.DownloadManager, templates *TemplateRenderer) *DatasetsView {
	return &DatasetsView{
		datasetsController: datasetsController,
		openFreeMapService: openFreeMapService,
		downloadManager:    downloadManager,
		templates:          templates,
	}
}

// DatasetItem represents a dataset with its status
type DatasetItem struct {
	Name       string
	Status     string  // "complete", "downloading", "error"
	Progress   float64 // 0-100 for downloading
	Error      string  // Error message if status is "error"
	Uncombined bool    // true if dataset needs combining
	IsActive   bool    // true if this is the currently active dataset
}

// DatasetsContext is the template context for datasets page
type DatasetsContext struct {
	BaseContext
	PageID         string
	PageName       string
	Datasets       []DatasetItem
	OpenFreeMapURL string
	HasUncombined  bool
	IsCombined     bool   // true if multiple datasets are combined
	ActiveDataset  string // name of active dataset (empty if combined)
}

// DatasetsAddContext is the template context for add dataset page
type DatasetsAddContext struct {
	BaseContext
	PageID   string
	PageName string
}

// DatasetsDeleteContext is the template context for delete dataset page
type DatasetsDeleteContext struct {
	BaseContext
	PageID   string
	PageName string
	Name     string
}

// Render handles GET /admin/datasets
func (v *DatasetsView) Render(w http.ResponseWriter, r *http.Request) {
	datasetNames, _ := v.datasetsController.GetDatasets()
	openFreeMapURL, _ := v.openFreeMapService.GetLatestPlanetURL()
	downloads := v.downloadManager.GetAllDownloads()
	activeDataset := v.datasetsController.GetActiveDataset()
	isCombined := v.datasetsController.IsCombined()

	// Build dataset items with status
	datasetMap := make(map[string]bool)
	var datasets []DatasetItem

	// Add existing datasets
	for _, name := range datasetNames {
		datasetMap[name] = true
		item := DatasetItem{
			Name:       name,
			Status:     "complete",
			Uncombined: v.datasetsController.IsUncombined(name),
			IsActive:   name == activeDataset,
		}
		// Check if there's a download in progress for this dataset
		if dl, ok := downloads[name]; ok {
			switch dl.Status {
			case services.DownloadStatusDownloading, services.DownloadStatusPending:
				item.Status = "downloading"
				item.Progress = dl.Progress
			case services.DownloadStatusError:
				item.Status = "error"
				item.Error = dl.Error
			}
		}
		datasets = append(datasets, item)
	}

	// Add downloads that aren't yet in the dataset list
	for name, dl := range downloads {
		if !datasetMap[name] {
			item := DatasetItem{
				Name: name,
			}
			switch dl.Status {
			case services.DownloadStatusDownloading, services.DownloadStatusPending:
				item.Status = "downloading"
				item.Progress = dl.Progress
			case services.DownloadStatusError:
				item.Status = "error"
				item.Error = dl.Error
			case services.DownloadStatusComplete:
				item.Status = "complete"
			}
			datasets = append(datasets, item)
		}
	}

	ctx := DatasetsContext{
		BaseContext:    NewBaseContext(),
		PageID:         "datasets",
		PageName:       "Datasets",
		Datasets:       datasets,
		OpenFreeMapURL: openFreeMapURL,
		HasUncombined:  v.datasetsController.HasUncombined(),
		IsCombined:     isCombined,
		ActiveDataset:  activeDataset,
	}

	v.templates.Render(w, "datasets.html", ctx)
}

// RenderAdd handles GET /admin/datasets/add
func (v *DatasetsView) RenderAdd(w http.ResponseWriter, r *http.Request) {
	ctx := DatasetsAddContext{
		BaseContext: NewBaseContext(),
		PageID:      "datasets",
		PageName:    "Add Dataset",
	}

	v.templates.Render(w, "datasets_add.html", ctx)
}

// RenderDelete handles GET /admin/datasets/delete/:name
func (v *DatasetsView) RenderDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx := DatasetsDeleteContext{
		BaseContext: NewBaseContext(),
		PageID:      "datasets",
		PageName:    "Delete Dataset",
		Name:        name,
	}

	v.templates.Render(w, "datasets_delete.html", ctx)
}
