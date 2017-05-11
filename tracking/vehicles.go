package tracking

import (
	"encoding/json"
	"fmt"
	"gopkg.in/cas.v1"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"math"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"gopkg.in/mgo.v2/bson"
)

//  represents a single position observed for a Vehicle from the data feed.
type VehicleUpdate struct {
	VehicleID string    `json:"vehicleID"   bson:"vehicleID,omitempty"`
	Lat       string    `json:"lat"         bson:"lat"`
	Lng       string    `json:"lng"         bson:"lng"`
	Heading   string    `json:"heading"     bson:"heading"`
	Speed     string    `json:"speed"       bson:"speed"`
	Lock      string    `json:"lock"        bson:"lock"`
	Time      string    `json:"time"        bson:"time"`
	Date      string    `json:"date"        bson:"date"`
	Status    string    `json:"status"      bson:"status"`
	Created   time.Time `json:"created"     bson:"created"`
	Segment   string    `json:"segment" 		bson:"segment"` // the segment that a vehicle resides on
}

// Vehicle represents an object being tracked.
type Vehicle struct {
	VehicleID   string    `json:"vehicleID"   bson:"vehicleID,omitempty"`
	VehicleName string    `json:"vehicleName" bson:"vehicleName"`
	Created     time.Time `bson:"created"`
	Updated     time.Time `bson:"updated"`
	Active bool `json:"active"`
}

// Status contains a detailed message on the tracked object's status
type Status struct {
	Public  bool      `bson:"public"`
	Message string    `json:"message" bson:"message"`
	Created time.Time `bson:"created"`
	Updated time.Time `bson:"updated"`
}

var (
	// Match each API field with any number (+)
	//   of the previous expressions (\d digit, \. escaped period, - negative number)
	//   Specify named capturing groups to store each field from data feed
	dataRe    = regexp.MustCompile(`(?P<id>Vehicle ID:([\d\.]+)) (?P<lat>lat:([\d\.-]+)) (?P<lng>lon:([\d\.-]+)) (?P<heading>dir:([\d\.-]+)) (?P<speed>spd:([\d\.-]+)) (?P<lock>lck:([\d\.-]+)) (?P<time>time:([\d]+)) (?P<date>date:([\d]+)) (?P<status>trig:([\d]+))`)
	dataNames = dataRe.SubexpNames()
)
var lastUpdate time.Time;

// UpdateShuttles send a request to iTrak API, gets updated shuttle info, and
// finally store updated records in db.
func (App *App) UpdateShuttles(dataFeed string, updateInterval int) {
	var st time.Duration
	for {

		// Sleep for n seconds before updating again
		log.Debugf("sleeping for %v", st)
		time.Sleep(st)
		if st == 0 {
			// Initialize the sleep timer after the first sleep.  This lets us sleep during errors
			// when we 'continue' back to the top of the loop without waiting to sleep for the first
			// update run.
			st = time.Duration(updateInterval) * time.Second
		}

		// Make request to our tracking data feed
		resp, err := http.Get(dataFeed)
		if err != nil {
			log.Errorf("error getting data feed: %v", err)
			continue
		}

		// Read response body content
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Errorf("error reading data feed: %v", err)
			continue
		}

		// this cannot be deferred because that will never get garbage collected
		resp.Body.Close()

		delim := "eof"
		// split the body of response by delimiter
		vehiclesData := strings.Split(string(body), delim)
		// BUG: if the request fails, it will give undefined result

		// TODO: Figure out if this handles == 1 vehicle correctly or always assumes > 1.
		if len(vehiclesData) <= 1 {
			log.Warnf("found no vehicles delineated by '%s'", delim)
		}

		updated := 0
		// for parsed data, update each vehicle
		for i := 0; i < len(vehiclesData)-1; i++ {
			match := dataRe.FindAllStringSubmatch(vehiclesData[i], -1)[0]
			// Store named capturing group and matching expression as a key value pair
			result := map[string]string{}
			for i, item := range match {
				result[dataNames[i]] = item
			}

			// Create new vehicle update & insert update into database
			// add computation of segment that the shuttle resides on and the arrival time to next N stops [here]

			// convert KPH to MPH
			speedKMH, err := strconv.ParseFloat(strings.Replace(result["speed"], "spd:", "", -1), 64)
			if err != nil {
				log.Error(err)
				continue
			}
			speedMPH := KPHtoMPH(speedKMH)
			speedMPHString := strconv.FormatFloat(speedMPH, 'f', 5, 64)

			update := VehicleUpdate{
				VehicleID: strings.Replace(result["id"], "Vehicle ID:", "", -1),
				Lat:       strings.Replace(result["lat"], "lat:", "", -1),
				Lng:       strings.Replace(result["lng"], "lon:", "", -1),
				Heading:   strings.Replace(result["heading"], "dir:", "", -1),
				Speed:     speedMPHString,
				Lock:      strings.Replace(result["lock"], "lck:", "", -1),
				Time:      strings.Replace(result["time"], "time:", "", -1),
				Date:      strings.Replace(result["date"], "date:", "", -1),
				Status:    strings.Replace(result["status"], "trig:", "", -1),
				Created:   time.Now()}

			// convert updated time to local time
			loc, err := time.LoadLocation("America/New_York")
			if err != nil {
				log.Error(err.Error())
				continue
			}

			lastUpdate = time.Now().In(loc)

			if err := App.Updates.Insert(&update); err != nil {
				log.Errorf("error inserting vehicle update(%v): %v", update, err)
			} else {
				updated++
			}

			// here if parsing error, updated will be incremented, wait, the whole thing will crash, isn't it?
		}
		log.Infof("sucessfully updated %d/%d vehicles", updated, len(vehiclesData)-1)
	}
}

// VehiclesHandler finds all the vehicles in the database.
func (App *App) VehiclesHandler(w http.ResponseWriter, r *http.Request) {

	// Find all vehicles in database
	var vehicles []Vehicle
	err := App.Vehicles.Find(bson.M{}).All(&vehicles)
	// Handle query errors
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send each vehicle to client as JSON
	WriteJSON(w, vehicles)
}

// VehiclesCreateHandler adds a new vehicle to the database.
func (App *App) VehiclesCreateHandler(w http.ResponseWriter, r *http.Request) {
	if App.Config.Authenticate && !cas.IsAuthenticated(r) {
		return
	}

	// Create new vehicle object using request fields
	vehicle := Vehicle{}
	vehicle.Created = time.Now()
	vehicle.Updated = time.Now()
	vehicleData := json.NewDecoder(r.Body)
	err := vehicleData.Decode(&vehicle)
	// Error handling
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	// Store new vehicle under vehicles collection
	err = App.Vehicles.Insert(&vehicle)
	// Error handling
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (App *App) VehiclesEditHandler(w http.ResponseWriter, r *http.Request) {

}

func (App *App) VehiclesDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if App.Config.Authenticate && !cas.IsAuthenticated(r) {
		return
	}

	// Delete vehicle from Vehicles collection
	vars := mux.Vars(r)
	log.Debugf("deleting", vars["id"])
	err := App.Vehicles.Remove(bson.M{"vehicleID": vars["id"]})
	// Error handling
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Here's my view, keep every name the same meaning, otherwise, choose another.
// UpdatesHandler get the most recent update for each vehicle in the vehicles collection.
func (App *App) UpdatesHandler(w http.ResponseWriter, r *http.Request) {
	// Store updates for each vehicle
	var vehicles []Vehicle
	var updates []VehicleUpdate
	var update VehicleUpdate
	var vehicleUpdates []VehicleUpdate
	// Query all Vehicles
	err := App.Vehicles.Find(bson.M{}).All(&vehicles)
	// Handle errors
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	// Find recent updates for each vehicle
	for _, vehicle := range vehicles {
		// here, huge waste of computational power, you record every shit inside the Updates table and using sort, I don't know what the hell is going on
		err := App.Updates.Find(bson.M{"vehicleID": vehicle.VehicleID}).Sort("-created").Limit(20).All(&vehicleUpdates);
		update = vehicleUpdates[0]

		if err == nil {
			count := 0.0
			speed := 0.0
			for i, elem := range vehicleUpdates{
				if(time.Since(elem.Created).Minutes() < 5){
					val,_ := strconv.ParseFloat(vehicleUpdates[i].Speed,64);
					speed += val
					count += 1;
				}
			}
			if(count > 0 && speed/count > 5){
				updates = append(updates, update)
			}
		}
	}

	// Convert updates to JSON
	WriteJSON(w, updates) // it's good to take some REST in our server :)
}

// UpdateMessageHandler generates a message about an update for a vehicle
func (App *App) UpdateMessageHandler(w http.ResponseWriter, r *http.Request) {
	// For each vehicle/update, store message as a string
	var messages []string
	var message string
	var vehicles []Vehicle
	var update VehicleUpdate

	// Query all Vehicles
	err := App.Vehicles.Find(bson.M{}).All(&vehicles)
	// Handle errors
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	// Find recent updates and generate message
	for _, vehicle := range vehicles {
		// find 10 most recent records
		err := App.Updates.Find(bson.M{"vehicleID": vehicle.VehicleID}).Sort("-created").Limit(1).One(&update)
		if err == nil {
			// Use first 4 char substring of update.Speed
			speed := update.Speed
			if len(speed) > 4 {
				speed = speed[0:4]
			}
			//nextArrival := GetArrivalTime(&update, App.Routes, App.Stops)
			message = fmt.Sprintf("<b>%s</b><br/>Traveling %s at<br/> %s mph as of %s", vehicle.VehicleName, CardinalDirection(&update.Heading), speed, lastUpdate.Format("3:04:05pm") /*, nextArrival*/)
			messages = append(messages, message)
		}
	}
	// Convert to JSON
	WriteJSON(w, messages)
}

func (App *App) LastUpdatesForVehicle(vehicle *Vehicle, count int) (updates []VehicleUpdate) {
	err := App.Updates.Find(bson.M{"vehicleID": vehicle.VehicleID}).Sort("-created").Limit(count).All(&updates)
	if err != nil {
		log.Error(err)
	}
	return
}

func (App *App) GuessRouteForVehicle(vehicle *Vehicle) (route Route) {
	samples := 50
	var routes []Route
	err := App.Routes.Find(bson.M{}).All(&routes)
	if err != nil {
		log.Error(err)
	}

	routeDistances := make(map[string]float64)
	for _, route := range routes {
		routeDistances[route.ID] = 0
		// routeDistances[route.ID] = math.Inf(0)
	}

	updates := App.LastUpdatesForVehicle(vehicle, samples)

	for _, update := range updates {
		updateLatitude, err := strconv.ParseFloat(update.Lat, 64)
		if err != nil {
			log.Error(err)
		}
		updateLongitude, err := strconv.ParseFloat(update.Lng, 64)
		if err != nil {
			log.Error(err)
		}

		for _, route := range routes {
			nearestDistance := math.Inf(0)
			for _, coord := range route.Coords {
				distance := math.Sqrt(math.Pow(updateLatitude - coord.Lat, 2) +
					math.Pow(updateLongitude - coord.Lng, 2))
				if distance < nearestDistance {
					nearestDistance = distance
				}
			}
			routeDistances[route.ID] += nearestDistance
		}
	}

	minDistance := math.Inf(0)
	var minRouteID string
	for id := range routeDistances {
		distance := routeDistances[id] / float64(samples)
		if distance < minDistance {
			minDistance = distance
			minRouteID = id
		}
	}

	err = App.Routes.Find(bson.M{"id": minRouteID}).One(&route)
	if err != nil {
		log.Error(err)
	}

	return
}

func (App *App) NextStopForVehicle(vehicle *Vehicle) (nextStop string) {
	route := App.GuessRouteForVehicle(vehicle)
	log.Info(vehicle.VehicleName, ": ", route.Name)

	var stops []Stop

	err := App.Stops.Find(bson.M{"routeId": route.ID}).All(&stops)
	if err != nil {
		log.Error(err)
	}

	// nearest stop
	updates := App.LastUpdatesForVehicle(vehicle, 25)
	for _, update := range updates {
		updateLatitude, err := strconv.ParseFloat(update.Lat, 64)
		if err != nil {
			log.Error(err)
		}
		updateLongitude, err := strconv.ParseFloat(update.Lng, 64)
		if err != nil {
			log.Error(err)
		}
		nearestStopDistance := math.Inf(0)
		var nearestStop Stop
		for _, stop := range stops {
			distance := math.Sqrt(math.Pow(updateLatitude - stop.Lat, 2) +
				math.Pow(updateLongitude - stop.Lng, 2))
			if distance < nearestStopDistance {
				nearestStopDistance = distance
				nearestStop = stop
			}
		}
		log.Info(nearestStop.Name)
	}

	log.Print(stops)

	update := App.LastUpdatesForVehicle(vehicle, 1)[0]
	return update.Lat
}

func (App *App) AllVehicles() (vehicles []Vehicle) {
	err := App.Vehicles.Find(bson.M{}).All(&vehicles)
	if err != nil {
		log.Error(err)
	}
	return
}

func (App *App) NextStops() {
	for {
		for _, vehicle := range App.AllVehicles() {
			if vehicle.VehicleName == "Bus 96" {
				App.NextStopForVehicle(&vehicle)
			}
		}
		time.Sleep(time.Second * 10)
	}
}

// CardinalDirection figures out the cardinal direction of a vehicle's heading
func CardinalDirection(h *string) string {
	heading, err := strconv.ParseFloat(*h, 64)
	if err != nil {
		fmt.Println("ERROR", err.Error())
		return "North" // hmmmm
	}
	switch {
	case (heading >= 22.5 && heading < 67.5):
		return "North-East"
	case (heading >= 67.5 && heading < 112.5):
		return "East"
	case (heading >= 112.5 && heading < 157.5):
		return "South-East"
	case (heading >= 157.5 && heading < 202.5):
		return "South"
	case (heading >= 202.5 && heading < 247.5):
		return "South-West"
	case (heading >= 247.5 && heading < 292.5):
		return "West"
	case (heading >= 292.5 && heading < 337.5):
		return "North-West"
	default:
		return "North"
	}
}

// convert kmh to mph
func KPHtoMPH(kmh float64) (mph float64) {
	mph = kmh * 0.621371192
	return
}
