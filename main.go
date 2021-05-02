package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Arafatk/glot"
	"github.com/fukurin00/geo_routing_provider/msg"
	grid "github.com/fukurin00/geo_routing_provider/routing"

	cav "github.com/synerex/proto_cav"
	sxmqtt "github.com/synerex/proto_mqtt"
	api "github.com/synerex/synerex_api"
	sxutil "github.com/synerex/synerex_sxutil"
	"google.golang.org/protobuf/proto"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	astar "github.com/fukurin00/astar_golang"
	ros "github.com/fukurin00/go_ros_msg"
	pbase "github.com/synerex/synerex_proto"
)

const (
	resolution  float64 = 0.3
	robotRadius float64 = 0.35
	closeThresh float64 = 0.85

	mapFile  string = "map/willow_garage_v_edited.pgm"
	yamlFile string = "map/willow_garage_v_edited.yaml"
)

var (
	mode Mode = ASTAR3D

	mqttsrv         = flag.String("mqtt", "localhost", "MQTT Broker address")
	nodesrv         = flag.String("nodesrv", "127.0.0.1:9990", "node serv address")
	sxServerAddress string

	mapMetaUpdate               = false
	mapMeta       *grid.MapMeta = nil
	gridMap       *grid.GridMap = nil
	astarPlanner  *astar.Astar  //if 2d mode

	mu sync.Mutex

	//synerex client
	mqttClient  *sxutil.SXServiceClient
	routeClient *sxutil.SXServiceClient

	msgCh chan mqtt.Message

	plot2d *glot.Plot
	plot3d *glot.Plot
)

func init() {
	msgCh = make(chan mqtt.Message)
}

type Mode int

const (
	ASTAR2D Mode = iota //normal astar
	ASTAR3D             //original astar
)

func routeCallback(clt *sxutil.SXServiceClient, sp *api.Supply) {
	rcd := &cav.DestinationRequest{}
	err := proto.Unmarshal(sp.Cdata.Entity, rcd)
	if err != nil {
		log.Print(err)
	}
	log.Printf("receive dest request robot%d", rcd.RobotId)

	var jsonPayload []byte
	if mode == ASTAR3D {
		if gridMap == nil {
			log.Print("not receive gridMap yet ...")
			return
		}
		isx, isy := gridMap.Pos2Ind(float64(rcd.Current.X), float64(rcd.Current.Y))
		igx, igy := gridMap.Pos2Ind(float64(rcd.Destination.X), float64(rcd.Destination.Y))

		routei, err := gridMap.Plan(isx, isy, igx, igy)
		if err != nil {
			log.Print(err)
		} else {
			route := gridMap.Route2Pos(0, routei)
			jsonPayload, err = msg.MakePathMsg(route)
			if err != nil {
				log.Print(err)
			}
		}
	} else if mode == ASTAR2D {
		route, err := astarPlanner.Plan(float64(rcd.Current.X), float64(rcd.Current.Y), float64(rcd.Destination.X), float64(rcd.Destination.Y))
		if err != nil {
			log.Print(err)
		} else {
			jsonPayload, err = msg.MakePathMsg2D(route)
			if err != nil {
				log.Print(err)
			}
		}
	}
	topic := fmt.Sprintf("robot/path/%d", rcd.RobotId)
	mqttProt := sxmqtt.MQTTRecord{
		Topic:  topic,
		Record: jsonPayload,
	}
	out, err := proto.Marshal(&mqttProt)
	if err != nil {
		log.Print(err)
	}
	cout := api.Content{Entity: out}
	smo := sxutil.SupplyOpts{
		Name:  "robotRoute",
		Cdata: &cout,
	}
	_, err = mqttClient.NotifySupply(&smo)
	if err != nil {
		log.Print(err)
	} else {
		log.Printf("send path robot %d", rcd.RobotId)
	}
}

func subsclibeRouteSupply(client *sxutil.SXServiceClient) {
	ctx := context.Background()
	for {
		client.SubscribeSupply(ctx, routeCallback)
		reconnectClient(client)
	}
}

//synerex recconect to client
func reconnectClient(client *sxutil.SXServiceClient) {
	mu.Lock()
	if client.SXClient != nil {
		client.SXClient = nil
		log.Printf("Client reset \n")
	}
	mu.Unlock()
	time.Sleep(5 * time.Second) // wait 5 seconds to reconnect
	mu.Lock()
	if client.SXClient == nil {
		newClt := sxutil.GrpcConnectServer(sxServerAddress)
		if newClt != nil {
			// log.Printf("Reconnect server [%s]\n", s.SxServerAddress)
			client.SXClient = newClt
		}
	}
	mu.Unlock()
}

func handleMqttMessage() {
	for {
		msg := <-msgCh
		if !mapMetaUpdate {
			log.Print("updating global costmap..")
			mu.Lock()
			var occupancy ros.OccupancyGrid
			merr := json.Unmarshal(msg.Payload(), &occupancy)
			if merr != nil {
				log.Print(merr)
			} else {
				mapMeta = grid.LoadROSMap(occupancy, 50)
				maxT := grid.MaxTimeLength
				gridMap = grid.NewGridMap(*mapMeta, maxT, robotRadius)
				log.Print("global costmap updated")
				plot2d.AddPointGroup("costmap", "dots", gridMap.ConvertObjMap2Point())
				mapMetaUpdate = true
				plot2d.SavePlot("map/global_costmap.png")
			}
			mu.Unlock()
		}

	}
}

// listening MQTT topics.
func listenMQTTBroker() {
	var myHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
		msgCh <- msg
	}
	opts := mqtt.NewClientOptions()
	opts.AddBroker("tcp://" + *mqttsrv + ":1883") // currently only 1883 port.

	clt := mqtt.NewClient(opts)

	if token := clt.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("MQTT connection error: %s", token.Error())
	}

	if subscribeToken := clt.Subscribe("map/global_costmap", 0, myHandler); subscribeToken.Wait() && subscribeToken.Error() != nil {
		log.Fatalf("MQTT subscribe error: %s", subscribeToken.Error())
	}
}

func LoggingSettings(logFile string) {
	logfile, _ := os.OpenFile(logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	multiLogFile := io.MultiWriter(os.Stdout, logfile)
	log.SetFlags(log.Ldate | log.Ltime)
	log.SetOutput(multiLogFile)
}

func SetupSynerex() {
	channels := []uint32{pbase.MQTT_GATEWAY_SVC, pbase.ROUTING_SERVICE}
	srv, err := sxutil.RegisterNode(*nodesrv, "GeoRoutingProvider", channels, nil)
	if err != nil {
		log.Fatal("can not registar node")
	}
	log.Printf("connectiong server [%s]", srv)
	sxServerAddress = srv

	synerexClient := sxutil.GrpcConnectServer(srv)
	argJson1 := "{Client: GeoMQTT}"
	mqttClient = sxutil.NewSXServiceClient(synerexClient, pbase.MQTT_GATEWAY_SVC, argJson1)
	argJson2 := "{Client: GeoRoute}"
	routeClient = sxutil.NewSXServiceClient(synerexClient, pbase.ROUTING_SERVICE, argJson2)
}

func SetupStaticMap() {
	mapMeta, err := grid.ReadStaticMapImage(yamlFile, mapFile, closeThresh)
	if err != nil {
		log.Print("read map file errore: ", err)
	}
	objMap := mapMeta.GetObjectMap()
	if mode == ASTAR2D {
		plot2d.AddPointGroup("map", "dots", grid.Convert2DPoint(objMap))
		plot2d.SavePlot("map/raw_static_map.png")
		astarPlanner = astar.NewAstar(objMap, robotRadius, resolution)
		log.Print("load astar obj map")
	} else if mode == ASTAR3D {
		maxT := grid.MaxTimeLength
		gridMap = grid.NewGridMapReso(*mapMeta, maxT, robotRadius, resolution, objMap)
		err = plot2d.AddPointGroup("objmap", "dots", gridMap.ConvertObjMap2Point())
		if err != nil {
			log.Print("plot add group error: ", err)
		}
		err = plot2d.SavePlot("map/static_obj_map.png")
		if err != nil {
			log.Print("save map error: ", err)
		}
	}
}

func main() {
	go sxutil.HandleSigInt()
	wg := sync.WaitGroup{}
	flag.Parse()
	sxutil.RegisterDeferFunction(sxutil.UnRegisterNode)

	//logging configuration
	now := time.Now()
	LoggingSettings("log/" + now.Format("2006-01-02-15-4") + ".log")

	// connect to mqtt broker
	listenMQTTBroker()

	// Synerex Configuration
	SetupSynerex()

	// visualization configuration
	plot2d, _ = glot.NewPlot(2, false, false)
	plot3d, _ = glot.NewPlot(3, false, false)

	// load static map data
	SetupStaticMap()

	//start main function
	log.Print("start subscribing")
	go handleMqttMessage()
	go subsclibeRouteSupply(routeClient)
	wg.Add(1)
	wg.Wait()
}
