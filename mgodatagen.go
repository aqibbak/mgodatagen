package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/fatih/color"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/jessevdk/go-flags"
	"gopkg.in/cheggaaa/pb.v1"

	"github.com/feliixx/mgodatagen/rg"
)

const (
	version = "0.0.1" // current version of mgodatagen
)

// Collection struct storing global collection info
type Collection struct {
	// Database to use
	DB string `json:"database"`
	// Collection name in the database
	Name string `json:"name"`
	// Number of documents to insert in the collection
	Count int `json:"count"`
	// Schema of the documents for this collection
	Content map[string]rg.GeneratorJSON `json:"content"`
	// Compression level for a collection. Available for `WiredTiger` only.
	// can be none|snappy|zlib. Default is "snappy"
	CompressionLevel string `json:"compressionLevel"`
	// List of indexes to build on this collection
	Indexes []Index `json:"indexes"`
	// Sharding information for sharded collection
	ShardConfig ShardingConfig `json:"shardConfig"`
}

// Index struct used to create an index from `db.runCommand({"createIndexes": "collName", ...})`
type Index struct {
	Name                    string         `bson:"name"`
	Key                     bson.M         `bson:"key"`
	Unique                  bool           `bson:"unique,omitempty"`
	DropDups                bool           `bson:"dropDups,omitempty"`
	Background              bool           `bson:"background,omitempty"`
	Sparse                  bool           `bson:"sparse,omitempty"`
	Bits                    int            `bson:"bits,omitempty"`
	Min                     float64        `bson:"min,omitempty"`
	Max                     float64        `bson:"max,omitempty"`
	BucketSize              float64        `bson:"bucketSize,omitempty"`
	ExpireAfter             int            `bson:"expireAfterSeconds,omitempty"`
	Weights                 bson.M         `bson:"weights,omitempty"`
	DefaultLanguage         string         `bson:"default_language,omitempty"`
	LanguageOverride        string         `bson:"language_override,omitempty"`
	TextIndexVersion        int            `bson:"textIndexVersion,omitempty"`
	PartialFilterExpression bson.M         `bson:"partialFilterExpression,omitempty"`
	Collation               *mgo.Collation `bson:"collation,omitempty"`
}

// ShardingConfig struct that holds information to shard the collection
type ShardingConfig struct {
	ShardCollection  string         `bson:"shardCollection"`
	Key              bson.M         `bson:"key"`
	unique           bool           `bson:"unique"`
	NumInitialChunks int            `bson:"numInitialChunks,omitempty"`
	Collation        *mgo.Collation `bson:"collation,omitempty"`
}

// Create an array generator to generate x json documetns at the same time
func getGenerator(content map[string]rg.GeneratorJSON, batchSize int, shortNames bool) (*rg.ArrayGenerator, error) {
	// create the global generator, used to generate 1000 items at a time
	g, err := rg.NewGeneratorsFromMap(content, shortNames)
	if err != nil {
		return nil, fmt.Errorf("error while creating generators from config file:\n\tcause: %s", err.Error())
	}
	eg := rg.EmptyGenerator{K: "", NullPercentage: 0, T: 6}
	gen := &rg.ArrayGenerator{
		EmptyGenerator: eg,
		Size:           batchSize,
		Generator:      &rg.ObjectGenerator{EmptyGenerator: eg, Generators: g}}
	return gen, nil
}

// get a connection from Connection args
func connectToDB(conn *Connection) (*mgo.Session, error) {
	fmt.Printf("Connecting to mongodb://%s:%s\n\n", conn.Host, conn.Port)
	url := "mongodb://"
	if conn.UserName != "" && conn.Password != "" {
		url += conn.UserName + ":" + conn.Password + "@"
	}
	session, err := mgo.Dial(url + conn.Host + ":" + conn.Port)
	if err != nil {
		return nil, fmt.Errorf("connection failed:\n\tcause: %s", err.Error())
	}
	infos, err := session.BuildInfo()
	if err != nil {
		return nil, fmt.Errorf("couldn't get mongodb version:\n\tcause: %s", err.Error())
	}
	fmt.Printf("mongodb server version %s\ngit version %s\nOpenSSL version %s\n\n", infos.Version, infos.GitVersion, infos.OpenSSLVersion)
	result := struct {
		Ok     bool
		ErrMsg string
		Shards []bson.M
	}{}
	// if it's a sharded cluster, print the list of shards. Don't bother with the error
	// if cluster is not sharded / user not allowed to run command against admin db
	err = session.Run(bson.M{"listShards": 1}, &result)
	if err == nil && result.ErrMsg == "" {
		json, err := json.MarshalIndent(result.Shards, "", "  ")
		if err == nil {
			fmt.Printf("shard list: %v\n", string(json))
		}
	}
	return session, nil
}

// create a collection with specific options
func createCollection(coll *Collection, session *mgo.Session, indexOnly bool) (*mgo.Collection, error) {
	c := session.DB(coll.DB).C(coll.Name)
	// if indexOnly, just return the collection as it already exists
	if indexOnly {
		return c, nil
	}
	// drop the collection before inserting new document. Ignore the error
	// if the collection does not exists
	c.DropCollection()
	fmt.Printf("Creating collection %s...\n", coll.Name)
	// if a compression level is specified, explicitly create the collection with the selected
	// compression level
	if coll.CompressionLevel != "" {
		strEng := bson.M{"wiredTiger": bson.M{"configString": "block_compressor=" + coll.CompressionLevel}}
		err := c.Create(&mgo.CollectionInfo{StorageEngine: strEng})
		if err != nil {
			return nil, fmt.Errorf("coulnd't create collection with compression level %s:\n\tcause: %s", coll.CompressionLevel, err.Error())
		}
	}
	// if the collection has to be sharded
	if coll.ShardConfig.ShardCollection != "" {
		result := struct {
			ErrMsg string
			Ok     bool
		}{}
		// chack that the config is correct
		nm := c.Database.Name + "." + c.Name
		if coll.ShardConfig.ShardCollection != nm {
			return nil, fmt.Errorf("wrong value for 'shardConfig.shardCollection', should be <database>.<collection>: found %s, expected %s", coll.ShardConfig.ShardCollection, nm)
		}
		if len(coll.ShardConfig.Key) == 0 {
			return nil, fmt.Errorf("wrong value for 'shardConfig.key', can't be null and must be an object like {'_id': 'hashed'}, found: %v", coll.ShardConfig.Key)
		}
		// index to shard the collection
		index := Index{Name: "shardKey", Key: coll.ShardConfig.Key}
		err := c.Database.Run(bson.D{{Name: "createIndexes", Value: c.Name}, {Name: "indexes", Value: [1]Index{index}}}, &result)
		if err != nil {
			return nil, fmt.Errorf("couldn't create shard key with index config %v\n\tcause: %s", index.Key, err.Error())
		}
		if !result.Ok {
			return nil, fmt.Errorf("couldn't create shard key with index config %v\n\tcause: %s", index.Key, result.ErrMsg)
		}
		err = session.Run(coll.ShardConfig, &result)
		if err != nil {
			return nil, fmt.Errorf("couldn't create sharded collection. Make sure that sharding is enabled,\n see https://docs.mongodb.com/manual/reference/command/enableSharding/#dbcmd.enableSharding for details\n\tcause: %s", err.Error())
		}
		if !result.Ok {
			return nil, fmt.Errorf("couldn't create sharded collection \n\tcause: %s", result.ErrMsg)
		}
	}
	return c, nil
}

// insert documents in DB, and then close the session
func insertInDB(coll *Collection, c *mgo.Collection, shortNames bool) error {
	// number of document to insert in each bulkinsert. Default is 1000
	// as mongodb insert 1000 docs at a time max
	batchSize := 1000
	// number of routines inserting documents simultaneously in database
	nbInsertingGoRoutines := runtime.NumCPU()
	// size of the buffered channel for docs to insert
	docBufferSize := 3
	// for really small insert, use only one goroutine and reduce the buffered channel size
	if coll.Count < 3000 {
		batchSize = coll.Count
		nbInsertingGoRoutines = 1
		docBufferSize = 1
	}
	generator, err := getGenerator(coll.Content, batchSize, shortNames)
	if err != nil {
		return err
	}
	// To make insertion faster, buffer the generated documents
	// and push them to a channel. The channel stores 3 x 1000 docs by default
	record := make(chan []bson.M, docBufferSize)
	// A channel to get error from goroutines
	errs := make(chan error, 1)
	// use context to handle errors in goroutines. If an error occurs in a goroutine,
	// all goroutines should terminate and the buffered channel should be closed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// create a waitGroup to make sure all the goroutines
	// have ended before returning
	var wg sync.WaitGroup
	wg.Add(nbInsertingGoRoutines)
	// start a new progressbar to display progress in terminal
	bar := pb.StartNew(coll.Count)
	// start numCPU goroutines to bulk insert documents in MongoDB
	for i := 0; i < nbInsertingGoRoutines; i++ {
		go func() {
			defer wg.Done()
			for r := range record {
				// if an error occurs in one of the goroutine, 'return' is called which trigger
				// wg.Done() ==> the goroutine stops
				select {
				case <-ctx.Done():
					return
				default:
				}
				bulk := c.Bulk()
				bulk.Unordered()
				for i := range r {
					bulk.Insert(r[i])
				}
				_, err := bulk.Run()
				if err != nil {
					// if the bulk insert fails, push the error to the error channel
					// so that we can use it from the main thread
					select {
					case errs <- fmt.Errorf("exception occurred during bulk insert:\n\tcause: %s", err.Error()):
					default:
					}
					// cancel the context to terminate goroutine and stop the feeding of the
					// buffered channel
					cancel()
					return
				}
			}
		}()
	}
	// Create a rand.Rand object to generate our random values
	source := rg.NewRandSource()
	// counter for already generated documents
	count := 0
	// start []bson.M generation to feed the buffered channel
	for count < coll.Count {
		select {
		case <-ctx.Done(): // if an error occurred in one of the 'inserting' goroutines, close the channel
			close(record)
			bar.Finish()
			return <-errs
		default:
		}
		// if nb of remaining docs to insert < 1000, re generate a generator of smaller size
		if (coll.Count-count) < 1000 && coll.Count > 1000 {
			batchSize = coll.Count - count
			generator, err = getGenerator(coll.Content, batchSize, shortNames)
			if err != nil {
				close(record)
				bar.Finish()
				return err
			}
		}
		// push genrated []bson.M to the buffered channel
		record <- generator.Value(source).([]bson.M)
		count += batchSize
		bar.Set(count)
	}
	close(record)
	// wait for goroutines to end
	wg.Wait()
	bar.Finish()
	color.Green("Generating collection %s done\n", coll.Name)
	// if an error occurs in one of the goroutines, return this error,
	// otherwise return nil
	if ctx.Err() != nil {
		return <-errs
	}
	return ctx.Err()
}

// create index on generated collections. Use run command as there is no wrapper
// like dropIndexes() in current mgo driver.
func ensureIndex(coll *Collection, c *mgo.Collection) error {
	if len(coll.Indexes) == 0 {
		fmt.Printf("No index to build for collection %s\n\n", coll.Name)
		return nil
	}
	fmt.Printf("Building indexes for collection %s...\n", coll.Name)
	result := struct {
		ErrMsg string
		Ok     bool
	}{}
	// drop all the indexes of the collection
	err := c.Database.Run(bson.D{{Name: "dropIndexes", Value: c.Name}, {Name: "index", Value: "*"}}, &result)
	if err != nil {
		return fmt.Errorf("error while dropping index for collection %s:\n\tcause: %s", coll.Name, err.Error())
	}
	if !result.Ok {
		return fmt.Errorf("error while dropping index for collection %s:\n\tcause: %s", coll.Name, result.ErrMsg)
	}
	//create the new indexes
	err = c.Database.Run(bson.D{{Name: "createIndexes", Value: c.Name}, {Name: "indexes", Value: coll.Indexes}}, &result)
	if err != nil {
		return fmt.Errorf("error while building indexes for collection %s:\n\tcause: %s", coll.Name, err.Error())
	}
	if !result.Ok {
		return fmt.Errorf("error while building indexes for collection %s:\n\tcause: %s", coll.Name, result.ErrMsg)
	}
	color.Green("Building indexes for collection %s done\n\n", coll.Name)
	return nil
}

func printCollStats(c *mgo.Collection) error {
	stats := struct {
		Count      int64  `bson:"count"`
		AvgObjSize int64  `bson:"avgObjSize"`
		IndexSizes bson.M `bson:"indexSizes"`
	}{}
	err := c.Database.Run(bson.D{{Name: "collStats", Value: c.Name}, {Name: "scale", Value: 1024}}, &stats)
	if err != nil {
		return fmt.Errorf("couldn't get stats for collection %s \n\tcause: %s ", c.Name, err.Error())
	}
	indexString := ""
	for k, v := range stats.IndexSizes {
		indexString += fmt.Sprintf("%s %v KB\n\t\t    ", k, v)
	}
	fmt.Printf("Stats for collection %s:\n\t - doc count: %v\n\t - average object size: %v bytes\n\t - indexes: %s\n", c.Name, stats.Count, stats.AvgObjSize, indexString)
	return nil
}

// pretty print an array of bson.M documents
func prettyPrintBSONArray(coll *Collection, shortNames bool) error {
	g, err := rg.NewGeneratorsFromMap(coll.Content, shortNames)
	if err != nil {
		return fmt.Errorf("fail to prettyPrint JSON doc:\n\tcause: %s", err.Error())
	}
	generator := rg.ObjectGenerator{Generators: g}
	source := rg.NewRandSource()
	doc := generator.Value(source).(bson.M)
	json, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("fail to prettyPrint JSON doc:\n\tcause: %s", err.Error())
	}
	fmt.Printf("generated: %s", string(json))
	return nil
}

// print the error in red and exit
func printErrorAndExit(err error) {
	color.Red("ERROR: %s", err.Error())
	os.Exit(0)
}

// General struct that stores global options from command line args
type General struct {
	Help    bool `long:"help" description:"show this help message"`
	Version bool `short:"v" long:"version" description:"print the tool version and exit"`
}

// Connection struct that stores info on connection from command line args
type Connection struct {
	Host     string `short:"h" long:"host" value-name:"<hostname>" description:"mongodb host to connect to" default:"127.0.0.1"`
	Port     string `long:"port" value-name:"<port>" description:"server port" default:"27017"`
	UserName string `short:"u" long:"username" value-name:"<username>" description:"username for authentification"`
	Password string `short:"p" long:"password" value-name:"<password>" description:"password for authentification"`
}

// Config struct that stores info on config file from command line args
type Config struct {
	ConfigFile string `short:"f" long:"file" value-name:"<configfile>" description:"JSON config file. This field is required"`
	IndexOnly  bool   `short:"i" long:"indexonly" description:"If present, mgodatagen will just try to rebuild index"`
	ShortName  bool   `short:"s" long:"shortname" description:"If present, JSON keys in the documents will be reduced\n to the first two letters only ('name' => 'na')"`
}

// Options struct to store flags from CLI
type Options struct {
	Config     `group:"configuration"`
	Connection `group:"connection infos"`
	General    `group:"general"`
}

func main() {
	// Reduce the number of GC calls as we are generating lots of objects
	debug.SetGCPercent(2000)
	// struct to store command line args
	var options Options
	p := flags.NewParser(&options, flags.Default&^flags.HelpFlag)
	_, err := p.Parse()
	if err != nil {
		fmt.Println("try mgodatagen --help for more informations")
		os.Exit(0)
	}
	if options.Help {
		fmt.Printf("mgodatagen version %s\n\n", version)
		p.WriteHelp(os.Stdout)
		os.Exit(0)
	}
	// if -v|--version print version and exit
	if options.Version {
		fmt.Printf("mgodatagen version %s\n", version)
		os.Exit(0)
	}
	if options.ConfigFile == "" {
		printErrorAndExit(fmt.Errorf("No configuration file provided, try mgodatagen --help for more informations "))
	}
	// read the json config file
	file, err := ioutil.ReadFile(options.ConfigFile)
	if err != nil {
		printErrorAndExit(fmt.Errorf("File error: %s", err.Error()))
	}
	// map to a json object
	fmt.Println("Parsing configuration file...")
	var collectionList []Collection
	err = json.Unmarshal(file, &collectionList)
	if err != nil {
		printErrorAndExit(fmt.Errorf("Error in config.json, object / array / Date badly formatted: \n\n\t\t%s", err.Error()))
	}
	session, err := connectToDB(&options.Connection)
	if err != nil {
		printErrorAndExit(err)
	}
	defer session.Close()
	// iterate over collection config
	for _, v := range collectionList {
		if v.Name == "" || v.DB == "" {
			printErrorAndExit(fmt.Errorf("collection name and database name can't be empty"))
		}
		if v.Count == 0 {
			printErrorAndExit(fmt.Errorf("for collection %s, count has to be > 0", v.Name))
		}
		// create the collection
		c, err := createCollection(&v, session, options.IndexOnly)
		if err != nil {
			printErrorAndExit(err)
		}
		// insert docs in database
		if !options.IndexOnly {
			err = insertInDB(&v, c, options.ShortName)
			if err != nil {
				printErrorAndExit(err)
			}
		}
		// create indexes on the collection
		err = ensureIndex(&v, c)
		if err != nil {
			printErrorAndExit(err)
		}
		err = printCollStats(c)
		if err != nil {
			printErrorAndExit(err)
		}
	}
	color.Green("Done")
}
