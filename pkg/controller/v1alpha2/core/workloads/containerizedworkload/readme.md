1. appConfig apply containerizedWorkload
    
   1.1 childResource label:
        workloadDefinition:
        
        traitArray
        
   1.2 apply containerizedWorkload
   
   
   1.3 cleanUp containerizedWorkload
    
 
2. containerizedWorkload  apply childResource
    
   2.1 apply childResource
   
   2.2 cleanUp childResource
   
   
3. trait
    3.1 volumeTrait
        get volumeTrait namespaceKey
        
        fetchWorkload
        
        fetchWorkloadChildResources
        
        patch childResource
        
           
    3.2 serviceTrait
    

    
    
    
