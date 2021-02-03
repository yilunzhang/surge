package surge

import (
	"sync"
	"time"

	"github.com/rule110-io/surge/backend/models"
	"github.com/rule110-io/surge/backend/platform"

	bitmap "github.com/boljen/go-bitmap"
	movavg "github.com/mxmCherry/movavg"
	"github.com/wailsapp/wails"
)

//FrontendReady is a flag to check if frontend is ready
var FrontendReady = false
var subscribers []string

var localFileName string
var sendSize int64
var receivedSize int64

var startTime = time.Now()

var clientOnlineMap map[string]bool

var downloadBandwidthAccumulator map[string]int
var uploadBandwidthAccumulator map[string]int

var fileBandwidthMap map[string]models.BandwidthMA

var zeroBandwidthMap map[string]bool

var chunksInTransit map[string]bool

var clientOnlineMapLock = &sync.Mutex{}
var clientOnlineRefreshingLock = &sync.Mutex{}
var bandwidthAccumulatorMapLock = &sync.Mutex{}
var chunkInTransitLock = &sync.Mutex{}

//Sessions .
//var Sessions []*sessionmanager.Session

//var testReader *bufio.Reader

var workerCount = 0

//Sessions collection lock
var sessionsWriteLock = &sync.Mutex{}

//ListedFilesLock lock this whenever you're reading or mutating the ListedFiles collection
var ListedFilesLock = &sync.Mutex{}

// File holds all info of a tracked file in surge
type File struct {
	FileName      string
	FileSize      int64
	FileHash      string
	Path          string
	NumChunks     int
	IsDownloading bool
	IsUploading   bool
	IsPaused      bool
	IsMissing     bool
	IsHashing     bool
	ChunkMap      []byte
	ChunksShared  int
	seeders       []string
	seederCount   int
}

//NumClientsStruct .
type NumClientsStruct struct {
	Online int
}

// FileListing struct for all frontend file listing props
type FileListing struct {
	FileName     string
	FileSize     int64
	FileHash     string
	Seeders      []string
	NumChunks    int
	IsTracked    bool
	IsAvailable  bool
	SeederCount  int
	ChunksShared int
}

// LocalFileListing is a wrapper for a local db file for the frontend
type LocalFileListing struct {
	FileName      string
	FileSize      int64
	FileHash      string
	Path          string
	NumChunks     int
	IsDownloading bool
	IsUploading   bool
	IsPaused      bool
	IsMissing     bool
	IsHashing     bool
	ChunksShared  int
	Seeders       []string
	SeederCount   int
	Progress      float32
}

//ListedFiles are remote files that can be downloaded
var ListedFiles []File

var wailsRuntime *wails.Runtime

var numClientsStore *wails.Store

// WailsBind is a binding function at startup
func WailsBind(runtime *wails.Runtime) {
	wailsRuntime = runtime
	platform.SetWailsRuntime(wailsRuntime, SetVisualMode)

	//Mac specific functions
	go platform.InitOSHandler()
	platform.SetVisualModeLikeOS()

	numClients := NumClientsStruct{
		Online: 0,
	}

	numClientsStore = wailsRuntime.Store.New("numClients", numClients)

	updateNumClientStore()

	//Wait for our client to initialize, perhaps there is no internet connectivity
	tryCount := 1
	for !clientInitialized {
		time.Sleep(time.Second)
		if tryCount%10 == 0 {
			pushError("Connection to NKN not yet established 0", "do you have an active internet connection?")
		}
		tryCount++
	}
	updateNumClientStore()

	//Get subs first synced then grab file queries for those subs
	GetSubscriptions()

	//Startup async processes to continue processing subs/files and updating gui
	go updateFileDataWorker()
	go rescanPeers()

	FrontendReady = true
}

func chunkMapFull(s []byte, num int) bool {
	//No chunkmap means no download was initiated, all chunks are local
	if s == nil {
		return true
	}

	for i := 0; i < num; i++ {
		if bitmap.Get(s, i) == false {
			return false
		}
	}
	return true
}

func chunksDownloaded(s []byte, num int) int {
	//No chunkmap means no download was initiated, all chunks are local
	if s == nil {
		return num
	}

	chunksLocalNum := 0
	for i := 0; i < num; i++ {
		if bitmap.Get(s, i) == true {
			chunksLocalNum++
		}
	}
	return chunksLocalNum
}

func fileBandwidth(FileID string) (Download int, Upload int) {

	//Get accumulator
	bandwidthAccumulatorMapLock.Lock()
	downAccu := downloadBandwidthAccumulator[FileID]
	downloadBandwidthAccumulator[FileID] = 0

	upAccu := uploadBandwidthAccumulator[FileID]
	uploadBandwidthAccumulator[FileID] = 0
	bandwidthAccumulatorMapLock.Unlock()

	if fileBandwidthMap[FileID].Download == nil {
		fileBandwidthMap[FileID] = models.BandwidthMA{
			Download: movavg.ThreadSafe(movavg.NewSMA(10)),
			Upload:   movavg.ThreadSafe(movavg.NewSMA(10)),
		}
	}

	fileBandwidthMap[FileID].Download.Add(float64(downAccu))
	fileBandwidthMap[FileID].Upload.Add(float64(upAccu))

	return int(fileBandwidthMap[FileID].Download.Avg()), int(fileBandwidthMap[FileID].Upload.Avg())
}
