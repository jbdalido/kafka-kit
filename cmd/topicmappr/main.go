package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/DataDog/topicmappr/kafkazk"

	"github.com/jamiealquiza/envy"
)

var (
	// Characters allowed in Kafka topic names
	topicNormalChar, _ = regexp.Compile(`[a-zA-Z0-9_\\-]`)

	// Config holds configuration
	// parameters.
	Config struct {
		rebuildMap      string
		rebuildTopics   []*regexp.Regexp
		brokers         []int
		useMeta         bool
		zkAddr          string
		zkPrefix        string
		zkMetricsPrefix string
		outPath         string
		outFile         string
		ignoreWarns     bool
		forceRebuild    bool
		replication     int
		subAffinity     bool
		placement       string
		optimize        string
		verbose         bool
	}
)

func init() {
	log.SetOutput(ioutil.Discard)

	flag.StringVar(&Config.rebuildMap, "rebuild-map", "", "Rebuild a topic map")
	topics := flag.String("rebuild-topics", "", "Rebuild topics (comma delim list) by lookup in ZooKeeper")
	flag.BoolVar(&Config.useMeta, "use-meta", true, "Use broker metadata in placement constraints")
	flag.StringVar(&Config.zkAddr, "zk-addr", "localhost:2181", "ZooKeeper connect string (for broker Metadata or rebuild-topic lookups)")
	flag.StringVar(&Config.zkPrefix, "zk-prefix", "", "ZooKeeper namespace prefix (for Kafka)")
	flag.StringVar(&Config.zkMetricsPrefix, "zk-metrics-prefix", "topicmappr", "ZooKeeper namespace prefix (for Kafka metrics)")
	flag.StringVar(&Config.outPath, "out-path", "", "Path to write output map files to")
	flag.StringVar(&Config.outFile, "out-file", "", "If defined, write a combined map of all topics to a file")
	flag.BoolVar(&Config.ignoreWarns, "ignore-warns", false, "Whether a map should be produced if warnings are emitted")
	flag.BoolVar(&Config.forceRebuild, "force-rebuild", false, "Forces a rebuild even if all existing brokers are provided")
	flag.IntVar(&Config.replication, "replication", 0, "Set the replication factor")
	flag.BoolVar(&Config.subAffinity, "sub-affinity", false, "Replacement broker substitution affinity")
	flag.StringVar(&Config.placement, "placement", "count", "Partition placement type: [count, storage]")
	flag.StringVar(&Config.optimize, "optimize", "distribution", "Optimization priority for storage placement: [distribution, storage]")
	flag.BoolVar(&Config.verbose, "verbose", false, "Verbose information")
	brokers := flag.String("brokers", "", "Broker list to rebuild topic partition map with")

	envy.Parse("TOPICMAPPR")
	flag.Parse()

	// Sanity check params.
	switch {
	case Config.rebuildMap == "" && *topics == "":
		fmt.Println("\n[ERROR] Must specify either -rebuild-map or -rebuild-topics")
		defaultsAndExit()
	case len(*brokers) == 0:
		fmt.Println("\n[ERROR] --brokers cannot be empty")
		defaultsAndExit()
	case Config.placement != "count" && Config.placement != "storage":
		fmt.Println("\n[ERROR] --placement must be either 'count' or 'storage'")
		defaultsAndExit()
	case Config.optimize != "distribution" && Config.optimize != "storage":
		fmt.Println("\n[ERROR] --optimize must be either 'distribution' or 'storage'")
		defaultsAndExit()
	case !Config.useMeta && Config.placement == "storage":
		fmt.Println("\n[ERROR] --placement=storage requires --use-meta=true")
		defaultsAndExit()
	case Config.forceRebuild && Config.subAffinity:
		fmt.Println("\n[INFO] --force-rebuild disables --sub-affinity")
	}

	// Append trailing slash if not included.
	if Config.outPath != "" && !strings.HasSuffix(Config.outPath, "/") {
		Config.outPath = Config.outPath + "/"
	}

	Config.brokers = kafkazk.BrokerStringToSlice(*brokers)
	topicNames := strings.Split(*topics, ",")

	// Determine if regexp was provided in the topic
	// name. If not, set the topic name to ^name$.
	for n, t := range topicNames {
		if !containsRegex(t) {
			topicNames[n] = fmt.Sprintf(`^%s$`, t)
		}
	}

	// Compile topic regex.
	for _, t := range topicNames {
		r, err := regexp.Compile(t)
		if err != nil {
			fmt.Printf("Invalid topic regex: %s\n", t)
			os.Exit(1)
		}

		Config.rebuildTopics = append(Config.rebuildTopics, r)
	}
}

func main() {
	// ZooKeeper init.
	zk := initZooKeeper()
	if zk != nil {
		defer zk.Close()
	}

	// General flow:
	// 1) A PartitionMap is formed (either unmarshaled from the literal
	//   map input via --rebuild-map or generated from ZooKeeper Metadata
	//   for topics matching --rebuild-topics).
	// 2) A BrokerMap is formed from brokers found in the PartitionMap
	//   along with any new brokers provided via the --brokers param.
	// 3) The PartitionMap and BrokerMap are fed to a rebuild
	//   function. Missing brokers, brokers marked for replacement,
	//   and all other placements are performed, returning a new
	//   PartitionMap.
	// 4) Differences between the original and new PartitionMap
	//   are detected and reported.
	// 5) The new PartitionMap is split by topic and a map is
	//   written for each.

	// Fetch broker and partition Metadata.
	brokerMeta := getbrokerMeta(zk)
	partitionMeta := getPartitionMeta(zk)

	// Build a topic map with either
	// text input or by fetching the
	// map data from ZooKeeper.
	// Store a copy of the original.
	partitionMapIn := getPartitionMap(zk)
	originalMap := partitionMapIn.Copy()

	// Get a list of affected topics.
	getTopics(partitionMapIn)

	brokers, brokersOrig, bs := getBrokers(partitionMapIn, brokerMeta)

	// Check if any referenced brokers are
	// marked as having missing/partial
	// metrics data.
	ensureBrokerMetrics(brokers, brokerMeta)

	// Create substitution affinities.
	affinities := getSubAffinities(brokers, brokersOrig, partitionMapIn)

	// Print changes, actions.
	printChangesActions(bs)

	// Apply any replication factor settings.
	updateReplicationFactor(partitionMapIn)

	// Build a new map using the provided list of brokers.
	// This is ok to run even when a no-op is intended.
	partitionMapOut, warns := buildMap(partitionMapIn, partitionMeta, brokers, affinities)

	// Sort by topic, partition.
	// TODO all functions should return
	// standard lex sorted partition maps.
	// Review for removal. Also, partitionMapIn
	// shouldn't be further referenced.
	sort.Sort(partitionMapIn.Partitions)
	sort.Sort(partitionMapOut.Partitions)

	// Count missing brokers as a warning.
	if bs.Missing > 0 {
		w := fmt.Sprintf("%d provided brokers not found in ZooKeeper\n", bs.Missing)
		warns = append(warns, w)
	}

	// Print warnings.
	fmt.Println("\nWARN:")
	if len(warns) > 0 {
		sort.Strings(warns)
		for _, e := range warns {
			fmt.Printf("%s%s\n", indent, e)
		}
	} else {
		fmt.Printf("%s[none]\n", indent)
	}

	// Print map change results.
	printMapChanges(originalMap, partitionMapOut)

	// Print broker assignment statistics.
	printBrokerAssignmentStats(originalMap, partitionMapOut, brokersOrig, brokers)

	// If no warnings were encountered, write out the
	// output topic map(s).
	if !Config.ignoreWarns && len(warns) > 0 {
		fmt.Printf("\n%sWarnings encountered, partition map not created. Override with --ignore-warns.\n", indent)
		os.Exit(1)
	}

	writeMaps(partitionMapOut)
}
