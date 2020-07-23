package surge

import (
	"bufio"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	bitmap "github.com/boljen/go-bitmap"
	nkn "github.com/nknorg/nkn-sdk-go"
	dialog "github.com/sqweek/dialog"
	"github.com/wailsapp/wails"
)

// SurgeActive is true when client is operational
var SurgeActive bool = false

//ChunkSize is size of chunk in bytes (256 kB)
const ChunkSize = 1024 * 256

//NumClients is the number of NKN clients
const NumClients = 8

//NumWorkers is the total number of concurrent chunk fetches allowed
const NumWorkers = 16

const localPath = "local"
const remotePath = "remote"

//OS folder permission bitflags
const (
	osRead       = 04
	osWrite      = 02
	osEx         = 01
	osUserShift  = 6
	osGroupShift = 3
	osOthShift   = 0

	osUserR   = osRead << osUserShift
	osUserW   = osWrite << osUserShift
	osUserX   = osEx << osUserShift
	osUserRw  = osUserR | osUserW
	osUserRwx = osUserRw | osUserX

	osGroupR   = osRead << osGroupShift
	osGroupW   = osWrite << osGroupShift
	osGroupX   = osEx << osGroupShift
	osGroupRw  = osGroupR | osGroupW
	osGroupRwx = osGroupRw | osGroupX

	osOthR   = osRead << osOthShift
	osOthW   = osWrite << osOthShift
	osOthX   = osEx << osOthShift
	osOthRw  = osOthR | osOthW
	osOthRwx = osOthRw | osOthX

	osAllR   = osUserR | osGroupR | osOthR
	osAllW   = osUserW | osGroupW | osOthW
	osAllX   = osUserX | osGroupX | osOthX
	osAllRw  = osAllR | osAllW
	osAllRwx = osAllRw | osGroupX
)

var localFileName string
var sendSize int64
var receivedSize int64

var startTime = time.Now()

var client *nkn.MultiClient

var clientOnlineMap map[string]bool

//Sessions .
var Sessions []*Session

//var testReader *bufio.Reader

var workerCount = 0

// File holds all info of a tracked file in surge
type File struct {
	FileName      string
	FileSize      int64
	FileHash      string
	Seeder        string
	Path          string
	NumChunks     int
	IsDownloading bool
	IsUploading   bool
	IsPaused      bool
	ChunkMap      []byte
}

// FileListing struct for all frontend file listing props
type FileListing struct {
	FileName    string
	FileSize    int64
	FileHash    string
	Seeder      string
	NumChunks   int
	IsTracked   bool
	IsAvailable bool
}

// Session is a wrapper for everything needed to maintain a surge session
type Session struct {
	FileHash        string
	FileSize        int64
	Downloaded      int64
	Uploaded        int64
	deltaDownloaded int64
	bandwidthQueue  []int
	session         net.Conn
	reader          *bufio.Reader
}

// DownloadStatusEvent holds update info on download progress
type DownloadStatusEvent struct {
	FileHash  string
	Progress  float32
	Status    string
	Bandwidth int
	NumChunks int
	ChunkMap  string
}

//ListedFiles are remote files that can be downloaded
var ListedFiles []File

var wailsRuntime *wails.Runtime

// Start initializes surge
func Start(runtime *wails.Runtime) {
	wailsRuntime = runtime
	var dirFileMode os.FileMode
	dirFileMode = os.ModeDir | (osUserRwx | osAllR)

	//Ensure local and remote folders exist
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		os.Mkdir(localPath, dirFileMode)
	}
	if _, err := os.Stat(remotePath); os.IsNotExist(err) {
		os.Mkdir(remotePath, dirFileMode)
	}

	account := InitializeAccount()

	var err error

	client, err = nkn.NewMultiClient(account, "", NumClients, false, nil)
	if err != nil {
		log.Fatal("If unexpected tell mutsi 0x0001", err)
	} else {
		<-client.OnConnect.C

		pushNotification("Client Connected", "Successfully connected to the NKN network")

		client.Listen(nil)
		SurgeActive = true
		go Listen()

		topicEncoded := TopicEncode(TestTopic)

		clientOnlineMap = make(map[string]bool)

		//go ScanLocal()

		go BuildSeedString()

		sendSeedSubscription(topicEncoded, "Surge File Seeder")
		go GetSubscriptions(topicEncoded)

		tracked := GetTrackedFiles()
		for i := 0; i < len(tracked); i++ {
			go restartDownload(tracked[i].FileHash)
		}

		go updateGUI()

		go rescanPeers()
	}
}

func rescanPeers() {
	for true {
		var numOnline = 0
		//Count num online clients
		for _, value := range clientOnlineMap {
			if value == true {
				numOnline++
			}
		}
		wailsRuntime.Events.Emit("remoteClientsUpdate", len(clientOnlineMap), numOnline)
		time.Sleep(time.Minute)
		topicEncoded := TopicEncode(TestTopic)
		go GetSubscriptions(topicEncoded)
	}
}

func updateGUI() {
	for true {
		time.Sleep(time.Second)

		for _, session := range Sessions {
			if session.FileSize == 0 {
				continue
			}

			//Take bandwith delta
			deltaBandwidth := int(session.Downloaded - session.deltaDownloaded)
			session.deltaDownloaded = session.Downloaded

			//Append to queue
			session.bandwidthQueue = append(session.bandwidthQueue, deltaBandwidth)

			//Dequeue if queue > 10
			if len(session.bandwidthQueue) > 10 {
				session.bandwidthQueue = session.bandwidthQueue[1:]
			}

			var bandwidthMA10 = 0
			for i := 0; i < len(session.bandwidthQueue); i++ {
				bandwidthMA10 += session.bandwidthQueue[i]
			}
			bandwidthMA10 /= len(session.bandwidthQueue)

			fileInfo, err := dbGetFile(session.FileHash)
			if err != nil {
				log.Panicln(err)
			}

			if fileInfo.IsPaused {
				continue
			}

			statusEvent := DownloadStatusEvent{
				FileHash:  session.FileHash,
				Progress:  float32(float64(session.Downloaded) / float64(session.FileSize)),
				Status:    "Downloading",
				Bandwidth: bandwidthMA10,
				NumChunks: fileInfo.NumChunks,
				ChunkMap:  GetFileChunkMapString(session.FileHash, 400),
			}
			log.Println("Emitting downloadStatusEvent: ", statusEvent)
			wailsRuntime.Events.Emit("downloadStatusEvent", statusEvent)

			//Download completed
			/*var completed = true
			for i := 0; i < fileInfo.NumChunks; i++ {
				if bitmap.Get(fileInfo.ChunkMap, i) == false {
					completed = false
					break
				}
			}*/

			if session.Downloaded == session.FileSize {
				pushNotification("Download Finished", getListedFileByHash(session.FileHash).FileName)
				session.session.Close()

				fileEntry, err := dbGetFile(session.FileHash)
				if err != nil {
					log.Panicln(err)
				}
				fileEntry.IsDownloading = false
				dbInsertFile(*fileEntry)
			}
		}
	}
}

//ByteCountSI converts filesize in bytes to human readable text
func ByteCountSI(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB",
		float64(b)/float64(div), "kMGTPE"[exp])
}

func getFileSize(path string) (size int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	// get the size
	return fi.Size()
}

func sendSeedSubscription(Topic string, Payload string) {
	txnHash, err := client.Subscribe("", Topic, 4320, Payload, nil)
	if err != nil {
		log.Println("Probably already subscribed", err)
	} else {
		log.Println("Subscribed: ", txnHash)
	}
}

//GetSubscriptions .
func GetSubscriptions(Topic string) {

	subscribers, err := client.GetSubscribers(Topic, 0, 100, true, true)
	if err != nil {
		log.Fatal("If unexpected tell mutsi 0x0002", err)
	}

	for k, v := range subscribers.SubscribersInTxPool.Map {
		subscribers.Subscribers.Map[k] = v
	}

	for k, v := range subscribers.Subscribers.Map {
		if len(v) > 0 {
			SendQueryRequest(k, "Testing query functionality.")
			clientOnlineMap[k] = false
		}
	}
}

// Stats .
type Stats struct {
	log *wails.CustomLogger
}

// WailsInit .
func (s *Stats) WailsInit(runtime *wails.Runtime) error {
	s.log = runtime.Log.New("Stats")
	runtime.Events.Emit("notificationEvent", "Backend Init", "just a test")
	log.Println("TESTING TESTING TESTING")
	return nil
}

func getListedFileByHash(Hash string) *File {
	for _, file := range ListedFiles {
		if file.FileHash == Hash {
			return &file
		}
	}
	return nil
}

//DownloadFile downloads the file
func DownloadFile(Hash string) bool {
	//Addr string, Size int64, FileID string

	file := getListedFileByHash(Hash)
	if file == nil {
		log.Panic("No listed file with hash", Hash)
	}

	// Create a sessions
	surgeSession, err := createSession(file)
	if err != nil {
		log.Println("Could not create session for download", Hash)
		pushNotification("Download Session Failed", file.FileName)
		return false
	}
	go initiateSession(surgeSession)

	pushNotification("Download Started", file.FileName)

	// If the file doesn't exist allocate it
	var path = remotePath + "/" + file.FileName
	AllocateFile(path, file.FileSize)
	numChunks := int((file.FileSize-1)/int64(ChunkSize)) + 1

	//When downloading from remote enter file into db
	dbFile, err := dbGetFile(Hash)
	log.Println(dbFile)
	if err != nil {
		file.Path = path
		file.NumChunks = numChunks
		file.ChunkMap = bitmap.NewSlice(numChunks)
		file.IsDownloading = true
		dbInsertFile(*file)
	}

	//Create a random fetch sequence
	randomChunks := make([]int, numChunks)
	for i := 0; i < numChunks; i++ {
		randomChunks[i] = i
	}
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(randomChunks), func(i, j int) { randomChunks[i], randomChunks[j] = randomChunks[j], randomChunks[i] })

	downloadJob := func() {
		for i := 0; i < numChunks; i++ {
			//Pause if file is paused
			dbFile, err := dbGetFile(file.FileHash)
			for err == nil && dbFile.IsPaused {
				time.Sleep(time.Second * 5)
				dbFile, err = dbGetFile(file.FileHash)
				if err != nil {
					break
				}
			}

			workerCount++
			go RequestChunk(surgeSession, file.FileHash, int32(randomChunks[i]))

			for workerCount >= NumWorkers {
				time.Sleep(time.Millisecond)
			}
		}
	}
	go downloadJob()

	return true
}

func pushNotification(title string, text string) {
	log.Println("Emitting Event: ", "notificationEvent", title, text)
	wailsRuntime.Events.Emit("notificationEvent", title, text)
}

//SearchQueryResult is a paging query result for file searches
type SearchQueryResult struct {
	Result []FileListing
	Count  int
}

//LocalFilePageResult is a paging query result for tracked files
type LocalFilePageResult struct {
	Result []File
	Count  int
}

//SearchFile runs a paged query
func SearchFile(Query string, Skip int, Take int) SearchQueryResult {
	var results []FileListing

	for _, file := range ListedFiles {
		if strings.Contains(strings.ToLower(file.FileName), strings.ToLower(Query)) {

			result := FileListing{
				FileName:  file.FileName,
				FileHash:  file.FileHash,
				FileSize:  file.FileSize,
				Seeder:    file.Seeder,
				NumChunks: file.NumChunks,
			}

			tracked, err := dbGetFile(result.FileHash)
			if err == nil && tracked != nil {
				result.IsTracked = true
				result.IsAvailable = true

				//If any chunk is missing set available to false
				for i := 0; i < result.NumChunks; i++ {
					if bitmap.Get(tracked.ChunkMap, i) == false {
						result.IsAvailable = false
						break
					}
				}
			}

			results = append(results, result)
		}
	}

	left := Skip
	right := Skip + Take

	if left > len(results) {
		left = len(results)
	}

	if right > len(results) {
		right = len(results)
	}

	return SearchQueryResult{
		Result: results[left:right],
		Count:  len(results),
	}
}

//GetTrackedFiles returns all files tracked in surge client
func GetTrackedFiles() []File {
	return dbGetAllFiles()
}

//GetFileChunkMapString returns the chunkmap in hex for a file given by hash
func GetFileChunkMapString(Hash string, Size int) string {
	file, err := dbGetFile(Hash)
	if err != nil {
		return ""
	}

	outputSize := Size
	inputSize := file.NumChunks

	stepSize := float64(inputSize) / float64(outputSize)
	stepSizeInt := int(stepSize)

	var boolBuffer = ""
	if inputSize >= outputSize {

		for i := 0; i < outputSize; i++ {
			localCount := 0
			for j := 0; j < stepSizeInt; j++ {
				local := bitmap.Get(file.ChunkMap, int(float64(i)*stepSize)+j)
				if local {
					localCount++
				} else {
					boolBuffer += "0"
					break
				}
			}
			if localCount == stepSizeInt {
				boolBuffer += "1"
			}
		}
	} else {
		iter := float64(0)
		for i := 0; i < outputSize; i++ {
			local := bitmap.Get(file.ChunkMap, int(iter))
			if local {
				boolBuffer += "1"
			} else {
				boolBuffer += "0"
			}
			iter += stepSize
		}
	}
	return boolBuffer
	//return hex.EncodeToString(file.ChunkMap)
}

//SetFilePause sets a file IsPaused state for by file hash
func SetFilePause(Hash string, State bool) {
	fileWriteLock.Lock()
	file, err := dbGetFile(Hash)
	if err != nil {
		pushNotification("Failed To Pause", "Could not find the file to pause.")
		return
	}
	file.IsPaused = State
	dbInsertFile(*file)
	fileWriteLock.Unlock()

	msg := "Paused"
	if State == false {
		msg = "Resumed"
	}
	pushNotification("Download "+msg, file.FileName)
}

//OpenFileDialog uses platform agnostic package for a file dialog
func OpenFileDialog() (string, error) {
	return dialog.File().Load()
}
