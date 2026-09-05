if type != "object" then
  error("worker Deployment must be an object")
elif (.spec | type) != "object" then
  error("worker Deployment .spec must be an object")
elif (.spec.replicas | type) != "number" then
  error("worker Deployment .spec.replicas must be a number")
elif (.spec.replicas | floor) != .spec.replicas then
  error("worker Deployment .spec.replicas must be an integer")
elif .spec.replicas != 2 then
  error("worker Deployment .spec.replicas must equal 2")
else
  .spec.replicas
end
